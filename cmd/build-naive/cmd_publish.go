package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

const canonicalModulePath = "github.com/sagernet/cronet-go"

var (
	publishBranch     string
	publishModuleBase string
)

var commandPublish = &cobra.Command{
	Use:   "publish",
	Short: "Commit to go branch and push",
	Run: func(cmd *cobra.Command, args []string) {
		publish()
	},
}

func init() {
	commandPublish.Flags().StringVar(&publishBranch, "branch", "go", "Target branch to publish to")
	commandPublish.Flags().StringVar(&publishModuleBase, "module-base", "", "Module download path (defaults to origin; does not change import paths)")
	mainCommand.AddCommand(commandPublish)
}

func publish() {
	log.Printf("Publishing to %s branch...", publishBranch)
	if publishModuleBase != "" {
		moduleBase = publishModuleBase
	} else {
		remote := strings.TrimSpace(runCommandOutput(projectRoot, "git", "remote", "get-url", "origin"))
		var err error
		moduleBase, err = modulePathFromRemote(remote)
		if err != nil {
			log.Fatal(err)
		}
	}
	if err := module.CheckPath(moduleBase); err != nil {
		log.Fatal(err)
	}
	branchRef := "refs/heads/" + publishBranch
	runCommand(projectRoot, "git", "check-ref-format", branchRef)

	mainCommit := strings.TrimSpace(runCommandOutput(projectRoot, "git", "rev-parse", "HEAD"))

	temporaryDirectory, err := os.MkdirTemp("", "cronet-go-publish-")
	if err != nil {
		log.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() {
		runCommand(projectRoot, "git", "worktree", "remove", "--force", temporaryDirectory)
		os.RemoveAll(temporaryDirectory)
	}()

	runCommand(projectRoot, "git", "worktree", "add", "--detach", temporaryDirectory, "HEAD")
	// Keep the source snapshot in the index, but append to the published history.
	// A normal push then rejects concurrent updates instead of overwriting them.
	if strings.TrimSpace(runCommandOutput(projectRoot, "git", "ls-remote", "--heads", "origin", branchRef)) != "" {
		runCommand(temporaryDirectory, "git", "fetch", "--no-tags", "origin", branchRef)
		runCommand(temporaryDirectory, "git", "reset", "--soft", "FETCH_HEAD")
	}

	// Prepare both commits locally; publish only after the module graph validates.
	log.Print("Step 1: Preparing main module and lib submodules...")

	copyDirectory(filepath.Join(projectRoot, "lib"), filepath.Join(temporaryDirectory, "lib"))
	copyDirectory(filepath.Join(projectRoot, "include"), filepath.Join(temporaryDirectory, "include"))
	copyFile(filepath.Join(projectRoot, "include_cgo.go"), filepath.Join(temporaryDirectory, "include_cgo.go"))

	libDirectory := filepath.Join(temporaryDirectory, "lib")
	libEntries, err := os.ReadDir(libDirectory)
	if err != nil {
		log.Fatalf("failed to read lib directory: %v", err)
	}

	var builtTargets []string
	for _, entry := range libEntries {
		if entry.IsDir() {
			library := "libcronet.a"
			if strings.HasPrefix(entry.Name(), "windows_") {
				library = "libcronet.dll"
			}
			info, err := os.Stat(filepath.Join(libDirectory, entry.Name(), library))
			if err != nil || info.Size() == 0 {
				log.Fatalf("missing or empty native library for %s", entry.Name())
			}
			// Also accepts artifacts produced before the fork-path fix.
			runCommand(filepath.Join(libDirectory, entry.Name()), "go", "mod", "edit", "-module="+canonicalModulePath+"/lib/"+entry.Name())
			builtTargets = append(builtTargets, entry.Name())
		}
	}

	if len(builtTargets) == 0 {
		log.Fatal("no lib directories found")
	}

	// These generated submodules are standalone; they do not require the root module.
	runCommand(temporaryDirectory, "git", "add", "-f", "--", "lib", "include", "include_cgo.go")
	commitMessage := fmt.Sprintf("Build from %s", mainCommit[:8])
	runCommand(temporaryDirectory, "git", "commit", "-m", commitMessage)

	firstCommit := strings.TrimSpace(runCommandOutput(temporaryDirectory, "git", "rev-parse", "HEAD"))
	pseudoVersion := formatPseudoVersion(getCommitTime(temporaryDirectory, firstCommit), firstCommit)
	log.Printf("First commit: %s, pseudo-version: %s", firstCommit[:12], pseudoVersion)

	log.Print("Step 2: Generating all package...")
	generateAllPackage(temporaryDirectory, pseudoVersion, builtTargets)
	if err := tidyAllPackage(temporaryDirectory, pseudoVersion, builtTargets); err != nil {
		log.Fatal(err)
	}
	runCommand(temporaryDirectory, "git", "add", "--", "all")
	runCommand(temporaryDirectory, "git", "commit", "-m", "Generate all package")
	runCommand(temporaryDirectory, "git", "push", "origin", "HEAD:"+branchRef)

	log.Printf("Published to %s branch!", publishBranch)
}

func modulePathFromRemote(remote string) (string, error) {
	if strings.HasPrefix(remote, "git@github.com:") {
		remote = "https://github.com/" + strings.TrimPrefix(remote, "git@github.com:")
	}
	parsed, err := url.Parse(remote)
	if err != nil || parsed.Host != "github.com" {
		return "", fmt.Errorf("cannot derive a GitHub module download path from origin; specify --module-base")
	}
	path := strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git")
	if len(strings.Split(path, "/")) != 2 {
		return "", fmt.Errorf("origin must identify a GitHub owner/repository")
	}
	return "github.com/" + path, nil
}

// Tidy against the exact local snapshot before it is published. Restore remote
// replacements afterward so consumers never receive paths into the worktree.
func tidyAllPackage(directory, version string, targets []string) error {
	allDirectory := filepath.Join(directory, "all")
	goModPath := filepath.Join(allDirectory, "go.mod")
	modules := []string{canonicalModulePath}
	for _, target := range targets {
		modules = append(modules, canonicalModulePath+"/lib/"+target)
	}
	for _, path := range modules {
		relative := ".." + strings.TrimPrefix(path, canonicalModulePath)
		runCommand(allDirectory, "go", "mod", "edit", "-replace="+path+"="+relative)
	}
	command := exec.Command("go", "mod", "tidy")
	command.Dir = allDirectory
	command.Env = append(os.Environ(), "GOWORK=off")
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("tidy published modules: %w", err)
	}
	content, err := os.ReadFile(goModPath)
	if err != nil {
		return err
	}
	file, err := modfile.Parse(goModPath, content, nil)
	if err != nil {
		return err
	}
	for _, path := range modules {
		found := false
		for _, requirement := range file.Require {
			if requirement.Mod.Path == path && requirement.Mod.Version == version {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("tidy changed or removed published module %s@%s", path, version)
		}
		if err := file.DropReplace(path, ""); err != nil {
			return err
		}
		if moduleBase != canonicalModulePath {
			remote := moduleBase + strings.TrimPrefix(path, canonicalModulePath)
			if err := file.AddReplace(path, "", remote, version); err != nil {
				return err
			}
		}
	}
	file.Cleanup()
	content, err = file.Format()
	if err != nil {
		return err
	}
	return os.WriteFile(goModPath, content, 0o644)
}

func formatPseudoVersion(commitTime time.Time, commitHash string) string {
	timestamp := commitTime.UTC().Format("20060102150405")
	return fmt.Sprintf("v0.0.0-%s-%s", timestamp, commitHash[:12])
}

func getCommitTime(directory, commitHash string) time.Time {
	output := runCommandOutput(directory, "git", "show", "-s", "--format=%cI", commitHash)
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(output))
	if err != nil {
		log.Fatalf("failed to parse commit time: %v", err)
	}
	return t
}

func generateAllPackage(directory, pseudoVersion string, builtTargets []string) {
	allDirectory := filepath.Join(directory, "all")
	err := os.MkdirAll(allDirectory, 0o755)
	if err != nil {
		log.Fatalf("failed to create all directory: %v", err)
	}

	generateAllGoMod(allDirectory, pseudoVersion, builtTargets)

	for _, targetName := range builtTargets {
		generatePlatformImportFile(allDirectory, targetName)
	}

	log.Printf("Generated all package with %d platforms", len(builtTargets))
}

func generateAllGoMod(allDirectory, pseudoVersion string, builtTargets []string) {
	var builder strings.Builder
	fmt.Fprintf(&builder, "module %s/all\n\n", canonicalModulePath)
	builder.WriteString("go 1.20\n\n")
	builder.WriteString("require (\n")
	fmt.Fprintf(&builder, "\t%s %s\n", canonicalModulePath, pseudoVersion)
	for _, targetName := range builtTargets {
		fmt.Fprintf(&builder, "\t%s/lib/%s %s\n", canonicalModulePath, targetName, pseudoVersion)
	}
	builder.WriteString(")\n")

	goModPath := filepath.Join(allDirectory, "go.mod")
	err := os.WriteFile(goModPath, []byte(builder.String()), 0o644)
	if err != nil {
		log.Fatalf("failed to write go.mod: %v", err)
	}
}

func generatePlatformImportFile(allDirectory, targetName string) {
	buildTag := getBuildTagForTarget(targetName)
	packageName := strings.ReplaceAll(targetName, "-", "_")

	content := fmt.Sprintf(`//go:build %s

package all

import (
	_ "%s"
	_ "%s/lib/%s"
)
`, buildTag, canonicalModulePath, canonicalModulePath, targetName)

	fileName := packageName + ".go"
	filePath := filepath.Join(allDirectory, fileName)
	err := os.WriteFile(filePath, []byte(content), 0o644)
	if err != nil {
		log.Fatalf("failed to write %s: %v", fileName, err)
	}
}

// getBuildTagForTarget returns the build tag for a given target directory name.
// Directory names follow the pattern: {platform}_{goarch}[_simulator][_musl]
// where platform is goos (linux, darwin, windows, android) or tvos/ios for Apple platforms.
func getBuildTagForTarget(targetName string) string {
	parts := strings.Split(targetName, "_")
	if len(parts) < 2 {
		log.Fatalf("invalid target name: %s", targetName)
	}

	goos := parts[0]
	goarch := parts[1]

	isSimulator := false
	isMusl := false
	isTvOS := false

	for i := 2; i < len(parts); i++ {
		switch parts[i] {
		case "simulator":
			isSimulator = true
		case "musl":
			isMusl = true
		}
	}

	// Handle tvOS: directory prefix is "tvos" but GOOS is "ios"
	if goos == "tvos" {
		isTvOS = true
		goos = "ios"
	}

	// Handle iOS/tvOS with gomobile-compatible tags
	if goos == "ios" {
		tagParts := []string{"ios", goarch}

		if isTvOS {
			tagParts = append(tagParts, "tvos")
			if isSimulator {
				tagParts = append(tagParts, "tvossimulator")
			} else {
				tagParts = append(tagParts, "!tvossimulator")
			}
		} else {
			tagParts = append(tagParts, "!tvos")
			if isSimulator {
				tagParts = append(tagParts, "iossimulator")
			} else {
				tagParts = append(tagParts, "!iossimulator")
			}
		}

		return strings.Join(tagParts, " && ")
	}

	if isMusl {
		return fmt.Sprintf("%s && !android && %s && with_musl", goos, goarch)
	}

	if goos == "linux" {
		return fmt.Sprintf("%s && !android && %s && !with_musl", goos, goarch)
	}

	if goos == "darwin" {
		return fmt.Sprintf("%s && !ios && %s", goos, goarch)
	}

	// Windows: purego only
	if goos == "windows" {
		return fmt.Sprintf("%s && %s && with_purego", goos, goarch)
	}

	return fmt.Sprintf("%s && %s", goos, goarch)
}
