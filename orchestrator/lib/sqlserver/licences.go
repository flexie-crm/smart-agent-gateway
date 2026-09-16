// Package sqlserver is not code. It exists so that the licences of the third
// party source in this folder travel with that source rather than being copied
// somewhere else and left to drift.
//
// The files are embedded from the folders they belong to (antlr/, tsql/), so
// moving the code moves its licence, and deleting one without the other does not
// compile. internal/licences reads this and the console shows it.
package sqlserver

import _ "embed"

// Licence is one piece of third-party source held here, and the terms it is
// held under.
type Licence struct {
	Name    string
	Version string
	Licence string
	Text    string
	URL     string
}

//go:embed antlr/LICENSE
var antlrLicence string

//go:embed tsql/LICENSE.bytebase
var bytebaseLicence string

//go:embed tsql/LICENSE.grammar
var grammarLicence string

// Licences is everything in this folder that somebody else wrote, which is all
// of it.
var Licences = []Licence{
	{
		Name:    "ANTLR Go runtime",
		Version: "v4.13.1",
		Licence: "BSD-3-Clause",
		Text:    antlrLicence,
		URL:     "https://github.com/antlr4-go/antlr",
	},
	{
		Name:    "bytebase/parser (Transact-SQL)",
		Version: "57b6ef7a2640",
		Licence: "BSD-3-Clause",
		Text:    bytebaseLicence,
		URL:     "https://github.com/bytebase/parser",
	},
	{
		Name:    "grammars-v4 Transact-SQL grammar",
		Licence: "MIT",
		Text:    grammarLicence,
		URL:     "https://github.com/antlr/grammars-v4",
	},
}
