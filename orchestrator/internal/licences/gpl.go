package licences

import _ "embed"

// The GNU General Public License version 2, which MariaDB is distributed under.
//
// It is held here rather than fetched at build time because it has to be IN the
// application: the desktop editions carry MariaDB, and GPLv2 asks that the
// licence accompany the program. A copy pulled from the internet when somebody
// happens to build is not a copy that ships.
//
// This is the text MariaDB itself ships (its own COPYING file, verbatim).
//
//go:embed GPL-2.0.txt
var mariaDB string
