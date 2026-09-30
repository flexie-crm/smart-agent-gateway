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

// The notices the browser ships, which the desktop applications carry so the
// assistant can drive a real page.
//
// Chromium is under a BSD 3-clause grant, and that grant asks for one thing in
// return for shipping a binary: reproduce the notice in the materials that go
// with it. This is that notice, and the open source screen is where it arrives.
//
// It is the file the browser itself ships (its own LICENSE.headless_shell,
// verbatim), which is Chromium's own grant followed by the notices of every
// library compiled into it, a few hundred of them. Copying it rather than
// summarising it is the point: a summary is a new document somebody has to be
// right about, where a copy is the thing the authors wrote.
//
// It belongs to a VERSION. The browser is pinned in desktop/browser.json, and
// bumping that pin without recopying this file publishes the notices of a build
// nobody is running.
//
//go:embed chrome-headless-shell.txt
var headlessShell string

// Playwright's injected script, which the desktop applications carry inside the
// binary: it is what finds things on a page for the browser tool (KB/43).
//
// Apache 2.0, and its section 4(d) asks for something a bare licence file does
// not cover: if the work ships a NOTICE, a redistribution has to reproduce the
// attribution in it. Playwright ships one, and it is not boilerplate. It says
// the work is Microsoft's AND that it contains code derived from Puppeteer, so
// dropping it would silently drop a second upstream's credit.
//
// So this file is that NOTICE followed by the licence, both verbatim from the
// package the script came from.
//
//go:embed playwright.txt
var playwright string

// browserVersion is the build those notices belong to.
//
// Written here rather than read from desktop/browser.json, where the pin
// actually lives, because embedding cannot reach outside this module. So it is
// a copy, and a copy is only safe if something fails when it stops matching:
// TestTheBrowserVersionMatchesThePin reads the pin and asserts it. That is the
// same arrangement the machine tools use for desktop/link-tools.json, and for
// the same reason (two halves that ship together and silently disagreed once).
const browserVersion = "149.0.7827.55"
