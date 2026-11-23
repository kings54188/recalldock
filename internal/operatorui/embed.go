package operatorui

import _ "embed"

// logoPNG is the shipped brand mark, served to the local UI so the page works
// fully offline and never references an external asset host.
//
//go:embed assets/logo.png
var logoPNG []byte
