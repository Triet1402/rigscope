// Package rigscope embeds the project license for the standalone CLI.
// SPDX-License-Identifier: AGPL-3.0-only
package rigscope

import _ "embed"

// License is the complete GNU Affero General Public License version 3 text.
//
//go:embed LICENSE
var License string
