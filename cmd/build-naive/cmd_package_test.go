package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Exercise generated cgo directives with a real archive and a conflicting shared
// library. This catches both Go's flag validation and accidentally switching the
// static binding to libcronet.so when replacing the GNU -l:filename syntax.
func TestPackageStaticLibraryLink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses Linux shared-library selection")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler unavailable")
	}
	ar, err := exec.LookPath("ar")
	if err != nil {
		t.Skip("archiver unavailable")
	}
	t.Setenv("CGO_ENABLED", "1")
	t.Setenv("CGO_CFLAGS", "")
	t.Setenv("CGO_LDFLAGS", "")
	t.Setenv("CGO_LDFLAGS_ALLOW", "")
	t.Setenv("CC", cc)
	t.Setenv("GOOS", runtime.GOOS)
	t.Setenv("GOARCH", runtime.GOARCH)
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	for _, local := range []bool{true, false} {
		t.Run(fmt.Sprintf("local=%v", local), func(t *testing.T) {
			oldRoot, oldNaiveRoot, oldSrcRoot := projectRoot, naiveRoot, srcRoot
			t.Cleanup(func() {
				projectRoot, naiveRoot, srcRoot = oldRoot, oldNaiveRoot, oldSrcRoot
			})
			projectRoot = t.TempDir()
			naiveRoot = filepath.Join(projectRoot, "naiveproxy")
			srcRoot = filepath.Join(naiveRoot, "src")
			target := Target{OS: "linux", CPU: hostToCPU(runtime.GOARCH), GOOS: "linux", ARCH: runtime.GOARCH}
			libraryName := getLibraryDirectoryName(target)
			libraryDirectory := filepath.Join(projectRoot, "lib", libraryName)
			writePublicationFile(t, projectRoot, "go.mod", "module example.com/cronet-fixture\n\ngo 1.24.0\n")
			writePublicationFile(t, naiveRoot, "CHROMIUM_VERSION", "154.0.8037.49\n")
			writePublicationFile(t, srcRoot, filepath.Join(getOutputDirectory(target), "obj/components/cronet/cronet_sample.ninja"), "libs =\n")
			writePublicationFile(t, libraryDirectory, "build/static.c", "int cronet_fixture_answer(void) { return 42; }\n")
			writePublicationFile(t, libraryDirectory, "build/shared.c", "int cronet_fixture_answer(void) { return 7; }\n")
			publicationCommand(t, libraryDirectory, cc, "-c", "build/static.c", "-o", "build/static.o")
			publicationCommand(t, libraryDirectory, ar, "rcs", "libcronet.a", "build/static.o")
			publicationCommand(t, libraryDirectory, cc, "-shared", "-fPIC", "build/shared.c", "-o", "libcronet.so")
			t.Setenv("LD_LIBRARY_PATH", libraryDirectory)

			directory, packageName := projectRoot, "cronet"
			if local {
				generateLocalCGOFiles([]Target{target})
			} else {
				generateSubmodules([]Target{target})
				directory, packageName = libraryDirectory, libraryName
			}
			writePublicationFile(t, directory, "fixture.go", fmt.Sprintf(`package %s
// int cronet_fixture_answer(void);
import "C"
func nativeAnswer() int { return int(C.cronet_fixture_answer()) }
`, packageName))
			writePublicationFile(t, directory, "fixture_test.go", fmt.Sprintf(`package %s
import "testing"
func TestStaticArchive(t *testing.T) {
	if got := nativeAnswer(); got != 42 {
		t.Fatalf("linked shared library instead of archive: %%d", got)
	}
}
`, packageName))
			publicationCommand(t, directory, "go", "test", "-count=1", ".")
		})
	}
}
