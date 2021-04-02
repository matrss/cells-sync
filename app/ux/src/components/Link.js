/**
 * Copyright 2019 Abstrium SAS
 *
 *  This file is part of Cells Sync.
 *
 *  Cells Sync is free software: you can redistribute it and/or modify
 *  it under the terms of the GNU General Public License as published by
 *  the Free Software Foundation, either version 3 of the License, or
 *  (at your option) any later version.
 *
 *  Cells Sync is distributed in the hope that it will be useful,
 *  but WITHOUT ANY WARRANTY; without even the implied warranty of
 *  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 *  GNU General Public License for more details.
 *
 *  You should have received a copy of the GNU General Public License
 *  along with Cells Sync.  If not, see <https://www.gnu.org/licenses/>.
 */
import React from 'react'
import { Link } from 'office-ui-fabric-react'

const generateLink = function ({ href, label }) {
    const lab = label || href;
    if (window.linkOpener) {
        return (<Link href={href} onClick={() => { window.linkOpener.open(href) }} target={"_blank"}>{lab}</Link>);
    } else {
        return (<Link href={href} target={"_blank"}>{lab}</Link>);
    }
}

export default generateLink;