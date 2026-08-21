package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	cacheDir   = "/var/cache/pkg"
	pkgDir     = "/mnt/disk/packages"
	repoIndex  = "https://raw.githubusercontent.com/semen88pochuev-eng/RezzOS-Packages/main/index.txt"
	arch       = "x86_64"
	certBundle = "/etc/ssl/certs/ca-certificates.crt"
)

var (
	blue   = "\033[0;34m"
	green  = "\033[0;32m"
	red    = "\033[0;31m"
	yellow = "\033[1;33m"
	nc     = "\033[0m"

	installedList string
	protectedFiles = []string{
		"/lib/ld-musl-x86_64.so.1",
		"/bin/busybox",
		"/sbin/busybox",
		"/usr/bin/busybox",
		"/sbin/init",
		"/sbin/runit",
		"/usr/sbin/runit",
		"/bin/runit",
		"/usr/bin/runsvdir",
		"/sbin/runsvdir",
	}
)

type Package struct {
	Name     string
	Version  string
	Arch     string
	URL      string
	SHA256   string
	Filename string
}

type InstalledPackage struct {
	Name     string
	Version  string
	Filename string
	SHA256   string
}

func init() {
	// Determine installed list location
	if isMounted("/mnt/disk") {
		installedList = filepath.Join(pkgDir, "installed.list")
		os.MkdirAll(pkgDir, 0755)
	} else {
		installedList = "/var/log/installed.list"
	}

	os.MkdirAll(cacheDir, 0755)
	os.MkdirAll("/tmp/pkg", 0755)
}

func isMounted(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == path {
			return true
		}
	}
	return false
}

func isProtected(path string) bool {
	for _, p := range protectedFiles {
		if path == p {
			return true
		}
	}
	return false
}

func usage() {
	fmt.Printf("%s=== RezzOS Package Manager (pkg) ===%s\n", blue, nc)
	fmt.Println("Usage:")
	fmt.Println("  pkg update                 - Refresh package index")
	fmt.Println("  pkg install <package>      - Install package")
	fmt.Println("  pkg upgrade                - Upgrade all installed packages")
	fmt.Println("  pkg search <query>         - Search for packages")
	fmt.Println("  pkg info <package>         - Show package details")
	fmt.Println("  pkg list                   - List installed packages")
	fmt.Println("  pkg remove <package>       - Remove package")
	fmt.Println("  pkg restore                - Re-place installed packages from cache (boot use)")
}

func downloadFile(url, output string) error {
	// Create HTTP client with custom transport
	transport := &http.Transport{}
	
	// Handle certificate bundle
	if _, err := os.Stat(certBundle); err != nil {
		fmt.Printf("%sWarning: no cert bundle at %s yet, HTTPS not verified for this download%s\n",
			yellow, certBundle, nc)
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	
	client := &http.Client{
		Transport: transport,
		Timeout:   0, // No timeout for large files
	}

	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Create output file
	out, err := os.Create(output)
	if err != nil {
		return err
	}
	defer out.Close()

	// Get total size
	total := resp.ContentLength

	// Progress bar
	done := make(chan bool)
	if total > 0 {
		go func() {
			barWidth := 30
			for {
				select {
				case <-done:
					return
				default:
					info, err := out.Stat()
					if err != nil {
						return
					}

					current := info.Size()
					percent := int(current * 100 / total)
					if percent > 100 {
						percent = 100
					}

					filled := percent * barWidth / 100
					empty := barWidth - filled

					bar := strings.Repeat("=", filled)
					if filled < barWidth {
						bar += ">"
						empty--
					}
					bar += strings.Repeat("-", empty)

					fmt.Printf("\r[%s] %3d%%", bar, percent)

					if percent >= 100 {
						fmt.Println()
						return
					}

					time.Sleep(time.Second)
				}
			}
		}()
	}

	_, err = io.Copy(out, resp.Body)
	close(done)
	
	if err != nil {
		os.Remove(output)
		return err
	}

	if total > 0 {
		fmt.Printf("\r[%s] 100%%\n", strings.Repeat("=", 30))
	} else {
		info, _ := out.Stat()
		fmt.Printf("\rDownloaded: %d bytes\n", info.Size())
	}
	
	return nil
}

func updateIndex() error {
	fmt.Print("Fetching index... ")
	indexPath := filepath.Join(cacheDir, "index.txt")
	if err := downloadFile(repoIndex, indexPath); err != nil {
		fmt.Printf("%sFAILED%s\n", red, nc)
		return err
	}
	fmt.Printf("%sOK%s\n", green, nc)
	return nil
}

func parseIndex() ([]Package, error) {
	indexPath := filepath.Join(cacheDir, "index.txt")
	file, err := os.Open(indexPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var packages []Package
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, "|")
		if len(parts) < 5 {
			continue
		}

		pkg := Package{
			Name:     parts[0],
			Version:  parts[1],
			Arch:     parts[2],
			URL:      parts[3],
			SHA256:   parts[4],
			Filename: filepath.Base(parts[3]),
		}

		if pkg.Arch == arch {
			packages = append(packages, pkg)
		}
	}

	return packages, scanner.Err()
}

func getLatestPackage(name string) (*Package, error) {
	packages, err := parseIndex()
	if err != nil {
		return nil, err
	}

	var latest *Package
	for i := range packages {
		if packages[i].Name == name {
			if latest == nil || packages[i].Version > latest.Version {
				latest = &packages[i]
			}
		}
	}

	return latest, nil
}

func searchPackages(query string) error {
	if query == "" {
		fmt.Printf("%sSpecify something to search for.%s\n", red, nc)
		return fmt.Errorf("empty query")
	}

	packages, err := parseIndex()
	if err != nil {
		fmt.Printf("%sNo index. Run 'pkg update' first.%s\n", red, nc)
		return err
	}

	fmt.Printf("%sSearching for '%s'...%s\n", yellow, query, nc)

	query = strings.ToLower(query)
	found := make(map[string]string)
	var names []string

	for _, pkg := range packages {
		if strings.Contains(strings.ToLower(pkg.Name), query) {
			if _, exists := found[pkg.Name]; !exists {
				found[pkg.Name] = pkg.Version
				names = append(names, pkg.Name)
			}
		}
	}

	if len(names) == 0 {
		fmt.Printf("%sNo package matches '%s'.%s\n", red, query, nc)
		return fmt.Errorf("not found")
	}

	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%-20s %s\n", name, found[name])
	}

	fmt.Printf("\n%s%d package(s) found.%s\n", green, len(names), nc)
	return nil
}

func infoPackage(name string) error {
	if name == "" {
		fmt.Printf("%sSpecify a package.%s\n", red, nc)
		return fmt.Errorf("empty name")
	}

	pkg, err := getLatestPackage(name)
	if err != nil || pkg == nil {
		fmt.Printf("%sNo package named '%s'.%s\n", red, name, nc)
		fmt.Printf("Try 'pkg search %s', or 'pkg update' if the index is stale.\n", name)
		return fmt.Errorf("not found")
	}

	fmt.Printf("\n  %s %s\n\n", pkg.Name, pkg.Version)
	fmt.Printf("  %-13s %s\n", "Architecture:", pkg.Arch)
	fmt.Printf("  %-13s %s\n", "Source:", pkg.URL)
	fmt.Printf("  %-13s %s\n", "SHA256:", pkg.SHA256)
	fmt.Println()

	return nil
}

func placePackageFiles(archive, name string) error {
	tmpDir, err := os.MkdirTemp("/tmp", "pkg_extract.*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	// Extract archive
	if err := extractTarGz(archive, tmpDir); err != nil {
		fmt.Printf("%sFailed to extract %s%s\n", red, filepath.Base(archive), nc)
		return err
	}

	// Get old owned files
	ownedFiles := make(map[string]bool)
	installedPkgs, _ := readInstalledList()
	for _, pkg := range installedPkgs {
		if pkg.Name == name {
			oldArchive := filepath.Join(pkgDir, pkg.Filename)
			if _, err := os.Stat(oldArchive); err == nil {
				if files, err := listArchiveFiles(oldArchive); err == nil {
					for _, f := range files {
						ownedFiles[f] = true
					}
				}
			}
			break
		}
	}

	// Walk through extracted files
	var skipped, failed int
	err = filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.Name() == ".PKGINFO" {
			return nil
		}

		if info.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(tmpDir, path)
		if err != nil {
			return err
		}

		target := "/" + rel

		if isProtected(target) {
			fmt.Printf("%sSkipped (protected system file): %s%s\n", yellow, target, nc)
			skipped++
			return nil
		}

		if _, err := os.Lstat(target); err == nil {
			if !ownedFiles[rel] {
				fmt.Printf("%sSkipped (already provided by another package or the base system): %s%s\n",
					yellow, target, nc)
				skipped++
				return nil
			}
		}

		// Create parent directory
		parentDir := filepath.Dir(target)
		if err := os.MkdirAll(parentDir, 0755); err != nil {
			fmt.Printf("%sFailed to create directory %s%s\n", red, parentDir, nc)
			failed++
			return nil
		}

		// Copy file with permissions
		if err := copyFile(path, target); err != nil {
			fmt.Printf("%sFailed to write %s%s\n", red, target, nc)
			failed++
		}

		return nil
	})

	if err != nil {
		return err
	}

	if skipped > 0 {
		fmt.Printf("%s%d file(s) skipped to avoid overwriting existing files%s\n", yellow, skipped, nc)
	}

	if failed > 0 {
		return fmt.Errorf("%d files failed to install", failed)
	}

	return nil
}

func installSinglePackage(name string) error {
	pkg, err := getLatestPackage(name)
	if err != nil || pkg == nil {
		fmt.Printf("%sNot found: %s%s\n", red, name, nc)
		return fmt.Errorf("not found")
	}

	fmt.Printf("%sInstalling %s %s...%s\n", yellow, pkg.Name, pkg.Version, nc)

	os.MkdirAll(pkgDir, 0755)
	os.MkdirAll("/tmp/pkg_dl", 0755)

	// Download package
	archivePath := filepath.Join("/tmp/pkg_dl", pkg.Filename)
	if err := downloadFile(pkg.URL, archivePath); err != nil {
		fmt.Printf("%sDownload failed for %s%s\n", red, name, nc)
		return err
	}

	// Verify SHA256
	actualSHA, err := calculateSHA256(archivePath)
	if err != nil {
		return err
	}

	if actualSHA != pkg.SHA256 {
		fmt.Printf("%sSHA256 mismatch for %s! Expected %s, got %s%s\n",
			red, name, pkg.SHA256, actualSHA, nc)
		os.Remove(archivePath)
		return fmt.Errorf("sha256 mismatch")
	}

	// Place files
	if err := placePackageFiles(archivePath, name); err != nil {
		fmt.Printf("%sSome files from %s failed to install%s\n", red, pkg.Filename, nc)
		return err
	}

	// Copy to package directory
	finalPath := filepath.Join(pkgDir, pkg.Filename)
	if err := copyFile(archivePath, finalPath); err != nil {
		fmt.Printf("%sFailed to save %s%s\n", red, pkg.Filename, nc)
		return err
	}

	// Update installed list
	if err := updateInstalledList(name, pkg.Version, pkg.Filename, pkg.SHA256); err != nil {
		fmt.Printf("%sFailed to update installed package list%s\n", red, nc)
		return err
	}

	fmt.Printf("%sDone: %s %s%s\n", green, name, pkg.Version, nc)
	return nil
}

func readInstalledList() ([]InstalledPackage, error) {
	file, err := os.Open(installedList)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var packages []InstalledPackage
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, "|")
		if len(parts) >= 4 {
			packages = append(packages, InstalledPackage{
				Name:     parts[0],
				Version:  parts[1],
				Filename: parts[2],
				SHA256:   parts[3],
			})
		}
	}

	return packages, scanner.Err()
}

func updateInstalledList(name, version, filename, sha256 string) error {
	packages, _ := readInstalledList()

	// Remove existing entry
	var newPackages []InstalledPackage
	for _, pkg := range packages {
		if pkg.Name != name {
			newPackages = append(newPackages, pkg)
		}
	}

	// Add new entry
	newPackages = append(newPackages, InstalledPackage{
		Name:     name,
		Version:  version,
		Filename: filename,
		SHA256:   sha256,
	})

	// Write back
	return writeInstalledList(newPackages)
}

func writeInstalledList(packages []InstalledPackage) error {
	file, err := os.Create(installedList)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	for _, pkg := range packages {
		fmt.Fprintf(writer, "%s|%s|%s|%s\n", pkg.Name, pkg.Version, pkg.Filename, pkg.SHA256)
	}

	return writer.Flush()
}

func restoreInstalled() error {
	packages, err := readInstalledList()
	if err != nil || len(packages) == 0 {
		fmt.Println("No packages to restore")
		return nil
	}

	ok, failed := 0, 0

	for _, pkg := range packages {
		archive := filepath.Join(pkgDir, pkg.Filename)

		if _, err := os.Stat(archive); err != nil {
			fmt.Printf("%sMissing cached archive for %s, skipping: %s%s\n", red, pkg.Name, archive, nc)
			failed++
			continue
		}

		// Verify SHA256
		if pkg.SHA256 != "" {
			actualSHA, err := calculateSHA256(archive)
			if err != nil || actualSHA != pkg.SHA256 {
				fmt.Printf("%sSHA256 mismatch for cached %s, skipping%s\n", red, pkg.Name, nc)
				failed++
				continue
			}
		}

		if err := placePackageFiles(archive, pkg.Name); err != nil {
			fmt.Printf("%sFailed to restore: %s%s\n", red, pkg.Name, nc)
			failed++
		} else {
			fmt.Printf("%sRestored: %s %s%s\n", green, pkg.Name, pkg.Version, nc)
			ok++
		}
	}

	fmt.Printf("%sRestored %d, failed %d%s\n", green, ok, failed, nc)
	if failed > 0 {
		return fmt.Errorf("%d packages failed to restore", failed)
	}
	return nil
}

func upgradePackages() error {
	packages, err := readInstalledList()
	if err != nil || len(packages) == 0 {
		fmt.Println("No packages")
		return nil
	}

	if _, err := os.Stat(filepath.Join(cacheDir, "index.txt")); err != nil {
		fmt.Printf("%sNo package index. Run 'pkg update' first.%s\n", red, nc)
		return err
	}

	checked, upgraded, failed := 0, 0, 0

	for _, pkg := range packages {
		checked++

		latest, err := getLatestPackage(pkg.Name)
		if err != nil || latest == nil {
			fmt.Printf("%sNot in index, skipping: %s%s\n", yellow, pkg.Name, nc)
			continue
		}

		if latest.Version == pkg.Version {
			continue
		}

		fmt.Printf("%s%s: %s -> %s%s\n", yellow, pkg.Name, pkg.Version, latest.Version, nc)

		oldFile := pkg.Filename
		if err := installSinglePackage(pkg.Name); err != nil {
			failed++
		} else {
			os.Remove(filepath.Join(pkgDir, oldFile))
			upgraded++
		}
	}

	fmt.Println()
	if upgraded == 0 && failed == 0 {
		fmt.Printf("%sEverything is up to date (%d checked)%s\n", green, checked, nc)
	} else {
		fmt.Printf("%sChecked %d, upgraded %d, failed %d%s\n", green, checked, upgraded, failed, nc)
	}

	if failed > 0 {
		return fmt.Errorf("%d packages failed to upgrade", failed)
	}
	return nil
}

func removePackages(names []string) error {
	if len(names) == 0 {
		fmt.Println("Specify package")
		return fmt.Errorf("no packages specified")
	}

	installedPkgs, _ := readInstalledList()

	for _, name := range names {
		var targetPkg *InstalledPackage
		for i := range installedPkgs {
			if installedPkgs[i].Name == name {
				targetPkg = &installedPkgs[i]
				break
			}
		}

		if targetPkg == nil {
			fmt.Printf("%sNot installed: %s%s\n", red, name, nc)
			continue
		}

		archivePath := filepath.Join(pkgDir, targetPkg.Filename)
		if _, err := os.Stat(archivePath); err != nil {
			fmt.Printf("%sPackage file not found: %s%s\n", red, targetPkg.Filename, nc)
			removeFromInstalledList(name)
			continue
		}

		fmt.Printf("%sRemoving %s...%s\n", yellow, name, nc)

		// Collect files owned by other packages
		keptFiles := make(map[string]bool)
		for _, pkg := range installedPkgs {
			if pkg.Name == name {
				continue
			}

			otherArchive := filepath.Join(pkgDir, pkg.Filename)
			if files, err := listArchiveFiles(otherArchive); err == nil {
				for _, f := range files {
					keptFiles[f] = true
				}
			}
		}

		// Get owned files
		ownedFiles, err := listArchiveFiles(archivePath)
		if err != nil {
			continue
		}

		shared := 0
		for _, file := range ownedFiles {
			if file == "" || file == ".PKGINFO" {
				continue
			}

			target := "/" + file
			if _, err := os.Lstat(target); err != nil {
				continue
			}

			if keptFiles[file] {
				shared++
				continue
			}

			os.Remove(target)
		}

		if shared > 0 {
			fmt.Printf("%sKept %d file(s) still owned by other packages%s\n", yellow, shared, nc)
		}

		os.Remove(archivePath)
		removeFromInstalledList(name)
		fmt.Printf("%sRemoved: %s%s\n", green, name, nc)
	}

	return nil
}

func removeFromInstalledList(name string) error {
	packages, _ := readInstalledList()

	var newPackages []InstalledPackage
	for _, pkg := range packages {
		if pkg.Name != name {
			newPackages = append(newPackages, pkg)
		}
	}

	return writeInstalledList(newPackages)
}

func listPackages() error {
	packages, err := readInstalledList()
	if err != nil || len(packages) == 0 {
		fmt.Println("No packages")
		return nil
	}

	for _, pkg := range packages {
		fmt.Printf("%-20s %s\n", pkg.Name, pkg.Version)
	}

	return nil
}

// Helper functions
func extractTarGz(archive, dest string) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		path := filepath.Join(dest, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(path, os.FileMode(header.Mode))
			os.Chmod(path, os.FileMode(header.Mode))
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(path), 0755)
			outFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			io.Copy(outFile, tarReader)
			outFile.Close()
			// Сохраняем права из архива
			os.Chmod(path, os.FileMode(header.Mode))
		case tar.TypeSymlink:
			os.Symlink(header.Linkname, path)
		}
	}

	return nil
}

func listArchiveFiles(archive string) ([]string, error) {
	file, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	var files []string
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		files = append(files, strings.TrimPrefix(header.Name, "./"))
	}

	return files, nil
}

func calculateSHA256(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	// Открыть исходный файл
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()

	// Получить информацию о правах
	sourceInfo, err := source.Stat()
	if err != nil {
		return err
	}

	// Создать файл с правами исходного
	destination, err := os.OpenFile(dst, 
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 
		sourceInfo.Mode().Perm())
	if err != nil {
		return err
	}
	defer destination.Close()

	// Копировать содержимое
	_, err = io.Copy(destination, source)
	if err != nil {
		return err
	}

	// Явно установить права (на случай если umask мешает)
	return os.Chmod(dst, sourceInfo.Mode().Perm())
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "update":
		err = updateIndex()
	case "install":
		if len(os.Args) < 3 {
			usage()
			os.Exit(1)
		}
		for _, pkg := range os.Args[2:] {
			if e := installSinglePackage(pkg); e != nil {
				err = e
			}
		}
	case "upgrade":
		err = upgradePackages()
	case "search":
		if len(os.Args) < 3 {
			fmt.Printf("%sSpecify something to search for.%s\n", red, nc)
			os.Exit(1)
		}
		err = searchPackages(os.Args[2])
	case "info":
		if len(os.Args) < 3 {
			fmt.Printf("%sSpecify a package.%s\n", red, nc)
			os.Exit(1)
		}
		err = infoPackage(os.Args[2])
	case "list":
		err = listPackages()
	case "remove":
		if len(os.Args) < 3 {
			fmt.Println("Specify package")
			os.Exit(1)
		}
		err = removePackages(os.Args[2:])
	case "restore":
		err = restoreInstalled()
	default:
		usage()
		os.Exit(1)
	}

	if err != nil {
		os.Exit(1)
	}
}
