package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

func TestModulePathFromRemote(t *testing.T) {
	for _, remote := range []string{
		"https://github.com/example/cronet-go.git",
		"https://github.com/example/cronet-go/",
		"git@github.com:example/cronet-go.git",
		"ssh://git@github.com/example/cronet-go.git",
	} {
		got, err := modulePathFromRemote(remote)
		if err != nil || got != "github.com/example/cronet-go" {
			t.Fatalf("%s: got %q, %v", remote, got, err)
		}
	}
	if _, err := modulePathFromRemote("/tmp/local-origin"); err == nil {
		t.Fatal("a local origin must require an explicit module download path")
	}
}

// Exercise the actual two-commit publisher against a local bare remote, then
// download the published modules through a file proxy as a downstream consumer.
// The fixture includes internal imports and all 29 ABI modules; no network is used.
func TestPublishForkRoundTrip(t *testing.T) {
	if source := os.Getenv("CRONET_PUBLISH_TEST_SOURCE"); source != "" {
		projectRoot, publishModuleBase, publishBranch = source, "github.com/example/cronet-go", "go"
		publish()
		return
	}
	directory := t.TempDir()
	remote := filepath.Join(directory, "remote.git")
	source := filepath.Join(directory, "source")
	proxy := filepath.Join(directory, "proxy")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("TMPDIR", directory)
	t.Setenv("GIT_AUTHOR_NAME", "Publisher Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "publisher@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Publisher Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "publisher@example.invalid")
	publicationCommand(t, directory, "git", "init", "--bare", remote)
	publicationCommand(t, directory, "git", "init", "-b", "main", source)
	publicationCommand(t, source, "git", "remote", "add", "origin", remote)
	writePublicationFile(t, source, "go.mod", "module "+canonicalModulePath+"\n\ngo 1.24.0\n")
	writePublicationFile(t, source, ".gitignore", "/lib/\n")
	writePublicationFile(t, source, "include_cgo.go", "package cronet\n")
	writePublicationFile(t, source, "include/dummy.go", "package include\n")
	writePublicationFile(t, source, "internal/cronet/engine.go", "package cronet\nconst Version = 150\n")
	writePublicationFile(t, source, "engine.go", `package cronet
import (
	"github.com/sagernet/cronet-go/internal/cronet"
	_ "github.com/sagernet/cronet-go/include"
)
const Version = cronet.Version
`)
	publicationCommand(t, source, "git", "add", ".")
	publicationCommand(t, source, "git", "commit", "-m", "Source")
	var targets []string
	for _, target := range allTargets {
		targets = append(targets, getLibraryDirectoryName(target))
	}
	for _, arch := range []string{"amd64", "arm64", "386", "arm", "loong64", "mipsle", "riscv64"} {
		targets = append(targets, "linux_"+arch+"_musl")
	}
	const fork = "github.com/example/cronet-go"
	for _, target := range targets {
		// Simulate artifacts from the earlier broken fork-path generator.
		writePublicationFile(t, source, "lib/"+target+"/go.mod", "module "+fork+"/lib/"+target+"\n\ngo 1.20\n")
		writePublicationFile(t, source, "lib/"+target+"/libcronet.go", "package native\n")
		library := "libcronet.a"
		if strings.HasPrefix(target, "windows_") {
			library = "libcronet.dll"
		}
		writePublicationFile(t, source, "lib/"+target+"/"+library, "native fixture")
	}
	oldRoot, oldBase, oldBranch := projectRoot, moduleBase, publishBranch
	oldPublishBase := publishModuleBase
	t.Cleanup(func() {
		projectRoot, moduleBase, publishBranch = oldRoot, oldBase, oldBranch
		publishModuleBase = oldPublishBase
	})
	projectRoot, publishModuleBase, publishBranch = source, fork, "go"
	publish()
	previous := strings.TrimSpace(publicationCommand(t, directory, "git", "--git-dir="+remote, "rev-parse", "go"))
	publish()
	publicationCommand(t, directory, "git", "--git-dir="+remote, "merge-base", "--is-ancestor", previous, "go")
	snapshot := filepath.Join(directory, "published")
	publicationCommand(t, directory, "git", "clone", "--branch", "go", remote, snapshot)
	baseCommit := strings.TrimSpace(publicationCommand(t, snapshot, "git", "rev-parse", "HEAD^"))
	allCommit := strings.TrimSpace(publicationCommand(t, snapshot, "git", "rev-parse", "HEAD"))
	baseVersion := formatPseudoVersion(getCommitTime(snapshot, baseCommit), baseCommit)
	allVersion := formatPseudoVersion(getCommitTime(snapshot, allCommit), allCommit)
	allMod := readPublicationMod(t, filepath.Join(snapshot, "all/go.mod"))
	if allMod.Module.Mod.Path != canonicalModulePath+"/all" || len(allMod.Replace) != 30 {
		t.Fatalf("all module must keep canonical imports and replace all 30 dependencies: %s", allMod.Module.Mod.Path)
	}
	for _, replacement := range allMod.Replace {
		want := fork + strings.TrimPrefix(replacement.Old.Path, canonicalModulePath)
		if replacement.New.Path != want || replacement.New.Version != baseVersion {
			t.Fatalf("unexpected published replacement: %+v", replacement)
		}
	}
	addPublicationProxyModule(t, proxy, snapshot, fork, baseVersion)
	for _, target := range targets {
		libraryDir := filepath.Join(snapshot, "lib", target)
		file := readPublicationMod(t, filepath.Join(libraryDir, "go.mod"))
		if file.Module.Mod.Path != canonicalModulePath+"/lib/"+target || len(file.Require) != 0 {
			t.Fatalf("lib/%s must be a standalone canonical module", target)
		}
		addPublicationProxyModule(t, proxy, libraryDir, fork+"/lib/"+target, baseVersion)
	}
	addPublicationProxyModule(t, proxy, filepath.Join(snapshot, "all"), fork+"/all", allVersion)
	consumer := filepath.Join(directory, "consumer")
	var consumerMod strings.Builder
	fmt.Fprintf(&consumerMod, "module example.com/consumer\n\ngo 1.24.0\n\nrequire %s/all %s\n", canonicalModulePath, allVersion)
	// Dependency replace directives are not inherited. Mirror sing-box/libcore.
	fmt.Fprintf(&consumerMod, "replace %s/all => %s/all %s\n", canonicalModulePath, fork, allVersion)
	for _, replacement := range allMod.Replace {
		fmt.Fprintf(&consumerMod, "replace %s => %s %s\n", replacement.Old.Path, replacement.New.Path, replacement.New.Version)
	}
	writePublicationFile(t, consumer, "go.mod", consumerMod.String())
	writePublicationFile(t, consumer, "main.go", "package main\nimport _ \""+canonicalModulePath+"/all\"\nfunc main() {}\n")
	proxyURL := url.URL{Scheme: "file", Path: filepath.ToSlash(proxy)}
	t.Setenv("GOPROXY", proxyURL.String())
	t.Setenv("GOMODCACHE", filepath.Join(directory, "modcache"))
	publicationCommand(t, consumer, "go", "mod", "tidy")
	publicationCommand(t, consumer, "go", "mod", "verify")
	publicationCommand(t, consumer, "go", "test", "-mod=readonly", "-tags", "with_purego", "./...")

	// A broken source snapshot must fail validation without replacing the good branch.
	writePublicationFile(t, source, "engine.go", "package cronet\nimport _ \"invalid.example/missing\"\n")
	publicationCommand(t, source, "git", "add", "engine.go")
	publicationCommand(t, source, "git", "commit", "-m", "Broken source fixture")
	child := exec.Command(os.Args[0], "-test.run=^TestPublishForkRoundTrip$")
	child.Env = append(os.Environ(), "CRONET_PUBLISH_TEST_SOURCE="+source)
	output, err := child.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "tidy published modules") {
		t.Fatalf("expected module validation to reject broken source: %v\n%s", err, output)
	}
	current := strings.TrimSpace(publicationCommand(t, directory, "git", "--git-dir="+remote, "rev-parse", "go"))
	if current != allCommit {
		t.Fatal("failed validation changed the published branch")
	}
}

func publicationCommand(t *testing.T, directory, name string, args ...string) string {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v in %s: %v\n%s", name, args, directory, err, output)
	}
	return string(output)
}

func writePublicationFile(t *testing.T, directory, name, content string) {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readPublicationMod(t *testing.T, path string) *modfile.File {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := modfile.Parse(path, content, nil)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func addPublicationProxyModule(t *testing.T, proxy, source, path, version string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(source, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	prefix := path + "/@v/" + version
	writePublicationFile(t, proxy, prefix+".mod", string(content))
	writePublicationFile(t, proxy, prefix+".info", fmt.Sprintf(`{"Version":%q,"Time":"2026-01-01T00:00:00Z"}`, version))
	file, err := os.Create(filepath.Join(proxy, prefix+".zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := modzip.CreateFromDir(file, module.Version{Path: path, Version: version}, source); err != nil {
		t.Fatal(err)
	}
}
