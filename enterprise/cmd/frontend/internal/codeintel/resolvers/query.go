package resolvers

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/opentracing/opentracing-go/log"
	"github.com/pkg/errors"

	store "github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/stores/dbstore"
	"github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/stores/lsifstore"
	"github.com/sourcegraph/sourcegraph/internal/observation"
)

var ErrMissingDump = errors.New("missing dump")

type ResolvedLocation struct {
	Dump  store.Dump
	Path  string
	Range lsifstore.Range
}

// AdjustedLocation is similar to a ResolvedLocation, but with fields denoting
// the commit and range adjusted for the target commit (when the requested commit is not indexed).
type AdjustedLocation struct {
	Dump           store.Dump
	Path           string
	AdjustedCommit string
	AdjustedRange  lsifstore.Range
}

// AdjustedDiagnostic is similar to a ResolvedDiagnostic, but with fields denoting
// the commit and range adjusted for the target commit (when the requested commit is not indexed).
type AdjustedDiagnostic struct {
	lsifstore.Diagnostic
	Dump           store.Dump
	AdjustedCommit string
	AdjustedRange  lsifstore.Range
}

// AdjustedCodeIntelligenceRange is similar to a CodeIntelligenceRange,
// but with adjusted definition and reference locations.
type AdjustedCodeIntelligenceRange struct {
	Range       lsifstore.Range
	Definitions []AdjustedLocation
	References  []AdjustedLocation
	HoverText   string
}

// QueryResolver is the main interface to bundle-related operations exposed to the GraphQL API. This
// resolver consolidates the logic for bundle operations and is not itself concerned with GraphQL/API
// specifics (auth, validation, marshaling, etc.). This resolver is wrapped by a symmetrics resolver
// in this package's graphql subpackage, which is exposed directly by the API.
type QueryResolver interface {
	Ranges(ctx context.Context, startLine, endLine int) ([]AdjustedCodeIntelligenceRange, error)
	Definitions(ctx context.Context, line, character int) ([]AdjustedLocation, error)
	References(ctx context.Context, line, character, limit int, rawCursor string) ([]AdjustedLocation, string, error)
	Hover(ctx context.Context, line, character int) (string, lsifstore.Range, bool, error)
	Diagnostics(ctx context.Context, limit int) ([]AdjustedDiagnostic, int, error)
}

type queryResolver struct {
	dbStore          DBStore
	lsifStore        LSIFStore
	gitserverClient  GitserverClient
	positionAdjuster PositionAdjuster
	repositoryID     int
	commit           string
	path             string
	uploads          []store.Dump
	operations       *operations
}

// NewQueryResolver create a new query resolver with the given services. The methods of this
// struct return queries for the given repository, commit, and path, and will query only the
// bundles associated with the given dump objects.
func NewQueryResolver(
	dbStore DBStore,
	lsifStore LSIFStore,
	gitserverClient GitserverClient,
	positionAdjuster PositionAdjuster,
	repositoryID int,
	commit string,
	path string,
	uploads []store.Dump,
	operations *operations,
) QueryResolver {
	return &queryResolver{
		dbStore:          dbStore,
		lsifStore:        lsifStore,
		gitserverClient:  gitserverClient,
		positionAdjuster: positionAdjuster,
		operations:       operations,
		repositoryID:     repositoryID,
		commit:           commit,
		path:             path,
		uploads:          uploads,
	}
}

const slowRangesRequestThreshold = time.Second

// Ranges returns code intelligence for the ranges that fall within the given range of lines. These
// results are partial and do not include references outside the current file, or any location that
// requires cross-linking of bundles (cross-repo or cross-root).
func (r *queryResolver) Ranges(ctx context.Context, startLine, endLine int) (_ []AdjustedCodeIntelligenceRange, err error) {
	ctx, endObservation := observeResolver(ctx, &err, "Ranges", r.operations.ranges, slowRangesRequestThreshold, observation.Args{
		LogFields: []log.Field{
			log.Int("repositoryID", r.repositoryID),
			log.String("commit", r.commit),
			log.String("path", r.path),
			log.String("uploadIDs", strings.Join(r.uploadIDs(), ", ")),
			log.Int("startLine", startLine),
			log.Int("endLine", endLine),
		},
	})
	defer endObservation()

	type TEMPORARY struct {
		Upload       store.Dump
		AdjustedPath string
		Ranges       []lsifstore.CodeIntelligenceRange
	}
	worklist := make([]TEMPORARY, 0, len(r.uploads))

	for _, upload := range r.uploads {
		// TODO - adjust pos
		adjustedPath, ok, err := r.positionAdjuster.AdjustPath(ctx, upload.Commit, r.path, false)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		worklist = append(worklist, TEMPORARY{
			Upload:       upload,
			AdjustedPath: adjustedPath,
		})
	}

	for i, w := range worklist {
		// TODO - batch these requests together
		ranges, err := r.lsifStore.Ranges(ctx, w.Upload.ID, strings.TrimPrefix(w.AdjustedPath, w.Upload.Root), startLine, endLine)
		if err != nil {
			return nil, err
		}

		worklist[i].Ranges = ranges
	}

	var ranges []AdjustedCodeIntelligenceRange
	for _, w := range worklist {
		for _, rn := range w.Ranges {
			_, adjustedRange, err := r.adjustRange(ctx, w.Upload.RepositoryID, w.Upload.Commit, w.AdjustedPath, rn.Range)
			if err != nil {
				return nil, err
			}

			adjustedDefinitions, err := r.adjustLocations(ctx, resolveLocationsWithDump(w.Upload, rn.Definitions))
			if err != nil {
				return nil, err
			}

			adjustedReferences, err := r.adjustLocations(ctx, resolveLocationsWithDump(w.Upload, rn.References))
			if err != nil {
				return nil, err
			}

			ranges = append(ranges, AdjustedCodeIntelligenceRange{
				Range:       adjustedRange,
				Definitions: adjustedDefinitions,
				References:  adjustedReferences,
				HoverText:   rn.HoverText,
			})
		}
	}

	return ranges, nil
}

const slowDefinitionsRequestThreshold = time.Second

// Definitions returns the list of source locations that define the symbol at the given position.
// This may include remote definitions if the remote repository is also indexed. If there are multiple
// bundles associated with this resolver, the definitions from the first bundle with any results will
// be returned.
func (r *queryResolver) Definitions(ctx context.Context, line, character int) (_ []AdjustedLocation, err error) {
	ctx, endObservation := observeResolver(ctx, &err, "Definitions", r.operations.definitions, slowDefinitionsRequestThreshold, observation.Args{
		LogFields: []log.Field{
			log.Int("repositoryID", r.repositoryID),
			log.String("commit", r.commit),
			log.String("path", r.path),
			log.String("uploadIDs", strings.Join(r.uploadIDs(), ", ")),
			log.Int("line", line),
			log.Int("character", character),
		},
	})
	defer endObservation()

	position := lsifstore.Position{
		Line:      line,
		Character: character,
	}

	type TEMPORARY struct {
		Upload           store.Dump
		AdjustedPath     string
		AdjustedPosition lsifstore.Position
		Locations        []lsifstore.Location
		OrderedMonikers  []lsifstore.MonikerData
	}
	var worklist []TEMPORARY

	for _, upload := range r.uploads {
		adjustedPath, adjustedPosition, ok, err := r.positionAdjuster.AdjustPosition(ctx, upload.Commit, r.path, position, false)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		worklist = append(worklist, TEMPORARY{
			Upload:           upload,
			AdjustedPath:     adjustedPath,
			AdjustedPosition: adjustedPosition,
		})
	}

	for i, w := range worklist {
		// TODO - batch these requests together
		locations, err := r.lsifStore.Definitions(ctx, w.Upload.ID, strings.TrimPrefix(w.AdjustedPath, w.Upload.Root), line, character)
		if err != nil {
			return nil, err
		}

		worklist[i].Locations = locations
	}

	for i, w := range worklist {
		if len(w.Locations) > 0 {
			break
		}

		// TODO - batch these requests together
		rangeMonikers, err := r.lsifStore.MonikersByPosition(
			ctx,
			w.Upload.ID, strings.TrimPrefix(w.AdjustedPath, w.Upload.Root),
			w.AdjustedPosition.Line,
			w.AdjustedPosition.Character,
		)
		if err != nil {
			return nil, err
		}

		var orderedMonikers []lsifstore.MonikerData
		for _, monikers := range rangeMonikers {
			for _, moniker := range monikers {
				if moniker.Kind == "import" && moniker.PackageInformationID != "" {
					orderedMonikers = append(orderedMonikers, moniker)
				}
			}
		}

		// TODO - ensure uniqueness
		worklist[i].OrderedMonikers = orderedMonikers
	}

	for i, w := range worklist {
		for _, moniker := range w.OrderedMonikers {
			// TODO - batch these requests together
			pid, _, err := r.lsifStore.PackageInformation(ctx, w.Upload.ID, strings.TrimPrefix(w.AdjustedPath, w.Upload.Root), string(moniker.PackageInformationID))
			if err != nil {
				return nil, err
			}

			dump, exists, err := r.dbStore.GetPackage(ctx, moniker.Scheme, pid.Name, pid.Version)
			if err != nil {
				return nil, err
			}
			if !exists {
				continue
			}

			const defintionMonikersLimit = 100
			locations, _, err := r.lsifStore.MonikerResults(ctx, dump.ID, "definitions", moniker.Scheme, moniker.Identifier, 0, defintionMonikersLimit)
			if err != nil {
				return nil, err
			}

			if len(locations) > 0 {
				worklist[i].Locations = locations
				break
			}
		}
	}

	for _, w := range worklist {
		if len(w.Locations) > 0 {
			adjustedLocations, err := r.adjustLocations(ctx, resolveLocationsWithDump(w.Upload, w.Locations))
			if err != nil {
				return nil, err
			}

			return adjustedLocations, nil
		}
	}

	return nil, nil
}

// ErrIllegalLimit occurs when a zero-length page of references is requested
var ErrIllegalLimit = errors.New("limit must be positive")

// remoteDumpLimit is the limit for fetching batches of remote dumps.
const remoteDumpLimit = 20

const slowReferencesRequestThreshold = time.Second

// References returns the list of source locations that reference the symbol at the given position.
// This may include references from other dumps and repositories. If there are multiple bundles
// associated with this resolver, results from all bundles will be concatenated and returned.
func (r *queryResolver) References(ctx context.Context, line, character, limit int, rawCursor string) (_ []AdjustedLocation, _ string, err error) {
	ctx, endObservation := observeResolver(ctx, &err, "References", r.operations.references, slowReferencesRequestThreshold, observation.Args{
		LogFields: []log.Field{
			log.Int("repositoryID", r.repositoryID),
			log.String("commit", r.commit),
			log.String("path", r.path),
			log.String("uploadIDs", strings.Join(r.uploadIDs(), ", ")),
			log.Int("line", line),
			log.Int("character", character),
		},
	})
	defer endObservation()

	position := lsifstore.Position{Line: line, Character: character}

	// Decode a map of upload ids to the next url that serves
	// the new page of results. This may not include an entry
	// for every upload if their result sets have already been
	// exhausted.
	cursors, err := readCursor(rawCursor)
	if err != nil {
		return nil, "", err
	}

	// We need to maintain a symmetric map for the next page
	// of results that we can encode into the endCursor of
	// this request.
	newCursors := map[int]string{}

	var allLocations []ResolvedLocation
	for i := range r.uploads {
		rawCursor := ""
		if cursor, ok := cursors[r.uploads[i].ID]; ok {
			rawCursor = cursor
		} else if len(cursors) != 0 {
			// Result set is exhausted or newer than the first page
			// of results. Skip anything from this upload as it will
			// have duplicate results, or it will be out of order.
			continue
		}

		adjustedPath, adjustedPosition, ok, err := r.positionAdjuster.AdjustPosition(ctx, r.uploads[i].Commit, r.path, position, false)
		if err != nil {
			return nil, "", err
		}
		if !ok {
			continue
		}

		cursor, err := DecodeOrCreateCursor(ctx, adjustedPath, adjustedPosition.Line, adjustedPosition.Character, r.uploads[i].ID, rawCursor, r.dbStore, r.lsifStore)
		if err != nil {
			return nil, "", err
		}

		if limit <= 0 {
			return nil, "", ErrIllegalLimit
		}

		locations, newCursor, hasNewCursor, err := NewReferencePageResolver(r.dbStore, r.lsifStore, r.gitserverClient, r.repositoryID, r.commit, remoteDumpLimit, limit).ResolvePage(ctx, cursor)
		if err != nil {
			return nil, "", err
		}

		allLocations = append(allLocations, locations...)
		if hasNewCursor {
			newCursors[r.uploads[i].ID] = EncodeCursor(newCursor)
		}
	}

	endCursor, err := makeCursor(newCursors)
	if err != nil {
		return nil, "", err
	}

	adjustedLocations, err := r.adjustLocations(ctx, allLocations)
	if err != nil {
		return nil, "", err
	}

	return adjustedLocations, endCursor, nil
}

const slowHoverRequestThreshold = time.Second

// Hover returns the hover text and range for the symbol at the given position. If there are
// multiple bundles associated with this resolver, the hover text and range from the first
// bundle with any results will be returned.
func (r *queryResolver) Hover(ctx context.Context, line, character int) (_ string, _ lsifstore.Range, _ bool, err error) {
	ctx, endObservation := observeResolver(ctx, &err, "Hover", r.operations.hover, slowHoverRequestThreshold, observation.Args{
		LogFields: []log.Field{
			log.Int("repositoryID", r.repositoryID),
			log.String("commit", r.commit),
			log.String("path", r.path),
			log.String("uploadIDs", strings.Join(r.uploadIDs(), ", ")),
			log.Int("line", line),
			log.Int("character", character),
		},
	})
	defer endObservation()

	position := lsifstore.Position{
		Line:      line,
		Character: character,
	}

	type TEMPORARY struct {
		Upload           store.Dump
		AdjustedPath     string
		AdjustedPosition lsifstore.Position
		Text             string
		Range            lsifstore.Range
	}
	var worklist []TEMPORARY

	for _, upload := range r.uploads {
		adjustedPath, adjustedPosition, ok, err := r.positionAdjuster.AdjustPosition(ctx, upload.Commit, r.path, position, false)
		if err != nil {
			return "", lsifstore.Range{}, false, err
		}
		if !ok {
			continue
		}

		worklist = append(worklist, TEMPORARY{
			Upload:           upload,
			AdjustedPath:     adjustedPath,
			AdjustedPosition: adjustedPosition,
		})
	}

	for i, w := range worklist {
		// TODO - batch these requests
		text, r, exists, err := r.lsifStore.Hover(ctx, w.Upload.ID, strings.TrimPrefix(w.AdjustedPath, w.Upload.Root), w.AdjustedPosition.Line, w.AdjustedPosition.Character)
		if err != nil {
			return "", lsifstore.Range{}, false, err
		}
		if !exists || text == "" {
			continue
		}

		worklist[i].Text = text
		worklist[i].Range = r
	}

	for _, w := range worklist {
		if w.Text != "" {
			_, adjustedRange, ok, err := r.positionAdjuster.AdjustRange(ctx, w.Upload.Commit, r.path, w.Range, true)
			if err != nil || !ok {
				return "", lsifstore.Range{}, false, err
			}

			return w.Text, adjustedRange, true, nil
		}
	}

	return "", lsifstore.Range{}, false, nil
}

const slowDiagnosticsRequestThreshold = time.Second

// Diagnostics returns the diagnostics for documents with the given path prefix. If there are
// multiple bundles associated with this resolver, results from all bundles will be concatenated
// and returned.
func (r *queryResolver) Diagnostics(ctx context.Context, limit int) (_ []AdjustedDiagnostic, _ int, err error) {
	ctx, endObservation := observeResolver(ctx, &err, "Diagnostics", r.operations.diagnostics, slowDiagnosticsRequestThreshold, observation.Args{
		LogFields: []log.Field{
			log.Int("repositoryID", r.repositoryID),
			log.String("commit", r.commit),
			log.String("path", r.path),
			log.String("uploadIDs", strings.Join(r.uploadIDs(), ", ")),
			log.Int("limit", limit),
		},
	})
	defer endObservation()

	type TEMPORARY struct {
		Upload       store.Dump
		AdjustedPath string
		Diagnostics  []lsifstore.Diagnostic
		Count        int
	}
	var worklist []TEMPORARY

	for _, upload := range r.uploads {
		adjustedPath, ok, err := r.positionAdjuster.AdjustPath(ctx, upload.Commit, r.path, false)
		if err != nil {
			return nil, 0, err
		}
		if !ok {
			continue
		}

		worklist = append(worklist, TEMPORARY{
			Upload:       upload,
			AdjustedPath: adjustedPath,
		})
	}

	for i, w := range worklist {
		// TODO - batch these requests
		diagnostics, count, err := r.lsifStore.Diagnostics(
			ctx,
			w.Upload.ID,
			strings.TrimPrefix(w.AdjustedPath, w.Upload.Root),
			0,
			limit,
		)
		if err != nil {
			return nil, 0, err
		}

		worklist[i].Diagnostics = diagnostics
		worklist[i].Count = count
	}

	totalCount := 0
	var adjustedDiagnostics []AdjustedDiagnostic
	for _, w := range worklist {
		for _, diagnostic := range w.Diagnostics {
			diagnostic = lsifstore.Diagnostic{
				DumpID:         diagnostic.DumpID,
				Path:           w.Upload.Root + diagnostic.Path,
				DiagnosticData: diagnostic.DiagnosticData,
			}

			clientRange := lsifstore.Range{
				Start: lsifstore.Position{Line: diagnostic.StartLine, Character: diagnostic.StartCharacter},
				End:   lsifstore.Position{Line: diagnostic.EndLine, Character: diagnostic.EndCharacter},
			}

			adjustedCommit, adjustedRange, err := r.adjustRange(ctx, w.Upload.RepositoryID, w.Upload.Commit, diagnostic.Path, clientRange)
			if err != nil {
				return nil, 0, err
			}

			adjustedDiagnostics = append(adjustedDiagnostics, AdjustedDiagnostic{
				Diagnostic:     diagnostic,
				Dump:           w.Upload,
				AdjustedCommit: adjustedCommit,
				AdjustedRange:  adjustedRange,
			})
		}

		totalCount += w.Count
	}

	if len(adjustedDiagnostics) > limit {
		adjustedDiagnostics = adjustedDiagnostics[:limit]
	}

	return adjustedDiagnostics, totalCount, nil
}

// uploadIDs returns a slice of this query's matched upload identifiers.
func (r *queryResolver) uploadIDs() []string {
	uploadIDs := make([]string, 0, len(r.uploads))
	for i := range r.uploads {
		uploadIDs = append(uploadIDs, strconv.Itoa(r.uploads[i].ID))
	}

	return uploadIDs
}

// adjustLocations translates a list of resolved locations (relative to the indexed commit) into a list of
// equivalent locations in the requested commit.
func (r *queryResolver) adjustLocations(ctx context.Context, locations []ResolvedLocation) ([]AdjustedLocation, error) {
	adjustedLocations := make([]AdjustedLocation, 0, len(locations))
	for i := range locations {
		adjustedCommit, adjustedRange, err := r.adjustRange(ctx, locations[i].Dump.RepositoryID, locations[i].Dump.Commit, locations[i].Path, locations[i].Range)
		if err != nil {
			return nil, err
		}

		adjustedLocations = append(adjustedLocations, AdjustedLocation{
			Dump:           locations[i].Dump,
			Path:           locations[i].Path,
			AdjustedCommit: adjustedCommit,
			AdjustedRange:  adjustedRange,
		})
	}

	return adjustedLocations, nil
}

// adjustRange translates a range (relative to the indexed commit) into an equivalent range in the requested commit.
func (r *queryResolver) adjustRange(ctx context.Context, repositoryID int, commit, path string, rx lsifstore.Range) (string, lsifstore.Range, error) {
	if repositoryID != r.repositoryID {
		// No diffs exist for translation between repos
		return commit, rx, nil
	}

	if _, adjustedRange, ok, err := r.positionAdjuster.AdjustRange(ctx, commit, path, rx, true); err != nil {
		return "", lsifstore.Range{}, err
	} else if ok {
		return r.commit, adjustedRange, nil
	}

	return commit, rx, nil
}

// readCursor decodes a cursor into a map from upload ids to URLs that serves the next page of results.
func readCursor(after string) (map[int]string, error) {
	if after == "" {
		return nil, nil
	}

	var cursors map[int]string
	if err := json.Unmarshal([]byte(after), &cursors); err != nil {
		return nil, err
	}
	return cursors, nil
}

// makeCursor encodes a map from upload ids to URLs that serves the next page of results into a single string
// that can be sent back for use in cursor pagination.
func makeCursor(cursors map[int]string) (string, error) {
	if len(cursors) == 0 {
		return "", nil
	}

	encoded, err := json.Marshal(cursors)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func resolveLocationsWithDump(dump store.Dump, locations []lsifstore.Location) []ResolvedLocation {
	var resolvedLocations []ResolvedLocation
	for _, location := range locations {
		resolvedLocations = append(resolvedLocations, ResolvedLocation{
			Dump:  dump,
			Path:  dump.Root + location.Path,
			Range: location.Range,
		})
	}

	return resolvedLocations
}
