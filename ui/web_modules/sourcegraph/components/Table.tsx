// tslint:disable

import * as React from "react";
import * as styles from "./styles/table.css";
import * as classNames from "classnames";

class Table extends React.Component<any, any> {
	static propTypes = {
		className: React.PropTypes.string,
		children: React.PropTypes.any,
		bordered: React.PropTypes.bool,
		style: React.PropTypes.object,
	};
	
	render(): JSX.Element | null {
		const {className, children, bordered, style} = this.props;

		return (
			<table className={classNames(className, bordered && styles.bordered)} style={style} cellSpacing="0">
				{children}
			</table>
		);
	}
}

export default Table;
