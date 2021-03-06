import MapSearchIcon from 'mdi-react/MapSearchIcon'
import React from 'react'
import { RouteComponentProps, Switch, Route } from 'react-router'

import { AuthenticatedUser } from '../auth'
import { withAuthenticatedUser } from '../auth/withAuthenticatedUser'
import { HeroPage } from '../components/HeroPage'
import { lazyComponent } from '../util/lazyComponent'

import { CreationRoutes } from './pages/creation/CreationRoutes'
import { SearchInsightCreationPageProps } from './pages/creation/search-insight/SearchInsightCreationPage'
import { InsightsPageProps } from './pages/dashboard/InsightsPage'
import { EditInsightPageProps } from './pages/edit/EditInsightPage'

const InsightsLazyPage = lazyComponent(() => import('./pages/dashboard/InsightsPage'), 'InsightsPage')
const EditInsightLazyPage = lazyComponent(() => import('./pages/edit/EditInsightPage'), 'EditInsightPage')

const NotFoundPage: React.FunctionComponent = () => <HeroPage icon={MapSearchIcon} title="404: Not Found" />

/**
 * This interface has to receive union type props derived from all child components
 * Because we need to pass all required prop from main Sourcegraph.tsx component to
 * sub-components withing app tree.
 */
export interface InsightsRouterProps
    extends RouteComponentProps,
        Omit<InsightsPageProps, 'isCreationUIEnabled'>,
        SearchInsightCreationPageProps,
        Omit<EditInsightPageProps, 'insightID'> {
    /**
     * Authenticated user info, Used to decide where code insight will appears
     * in personal dashboard (private) or in organisation dashboard (public)
     * */
    authenticatedUser: AuthenticatedUser
}

/**
 * Main Insight routing component. Main entry point to code insights UI.
 */
export const InsightsRouter = withAuthenticatedUser<InsightsRouterProps>(props => {
    const { match, ...outerProps } = props

    return (
        <Switch>
            <Route render={props => <InsightsLazyPage {...outerProps} {...props} />} path={match.url} exact={true} />

            <Route
                path={`${match.url}/create`}
                render={() => (
                    <CreationRoutes
                        platformContext={outerProps.platformContext}
                        authenticatedUser={outerProps.authenticatedUser}
                        settingsCascade={outerProps.settingsCascade}
                        telemetryService={outerProps.telemetryService}
                    />
                )}
            />

            <Route
                path={`${match.url}/edit/:insightID`}
                render={(props: RouteComponentProps<{ insightID: string }>) => (
                    <EditInsightLazyPage
                        platformContext={outerProps.platformContext}
                        authenticatedUser={outerProps.authenticatedUser}
                        settingsCascade={outerProps.settingsCascade}
                        insightID={props.match.params.insightID}
                    />
                )}
            />

            <Route component={NotFoundPage} key="hardcoded-key" />
        </Switch>
    )
})
