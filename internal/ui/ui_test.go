package ui

import "testing"

// The UI is embedded, so a missing asset is a build-time regression that must fail
// here rather than as a 404 in a browser.
func TestEmbeddedAssetsExist(t *testing.T) {
	for _, name := range []string{
		"index.html",
		"app.js",
		"styles.css",
		"vendor/uplot.iife.min.js",
		"vendor/uplot.min.css",
		"vendor/uplot.LICENSE",
	} {
		file, err := FS().Open(name)
		if err != nil {
			t.Fatalf("%s missing from the embedded assets: %v", name, err)
		}
		info, err := file.Stat()
		_ = file.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Size() == 0 {
			t.Fatalf("%s is empty", name)
		}
	}
}

func TestVendoredLibraryCarriesItsLicence(t *testing.T) {
	licence, err := FS().Open("vendor/uplot.LICENSE")
	if err != nil {
		t.Fatalf("uPlot licence not vendored: %v", err)
	}
	defer func() { _ = licence.Close() }()

	buf := make([]byte, 512)
	n, _ := licence.Read(buf)
	if n == 0 {
		t.Fatal("uPlot licence is empty")
	}
}
