package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
)

const (
	AppVersion = "1.0.0"                                       // Tracks the current version of the tool
	baseURL    = "https://update.talend.com/studio/8/updates/" // Base URL where to find the Qlik Talend Studio updates
	outputDir  = "releases"                                    // Directory to save downloaded releases
	maxRetries = 10                                            // Increased to 10 maximum attempts
	baseWait   = 2.0                                           // Base duration in seconds for exponential backoff
)

// Task represents a file download job
type Task struct {
	URL     string // Absolute URL to download the file from
	ZipPath string // Relative path inside the zip archive
}

// Global stats for progress tracking
var (
	totalBytesDownloaded int64
	filesDownloaded      int64
	totalFiles           int64
	startTime            time.Time
)

func main() {
	// 1. Calculate dynamic baseline for network workers based on host threads
	numCores := runtime.NumCPU()
	defaultWorkers := numCores * 3
	if defaultWorkers < 8 {
		defaultWorkers = 8
	} else if defaultWorkers > 24 {
		defaultWorkers = 24
	}

	// 2. Setup CLI Flags
	listFlag := flag.Bool("list", false, "List all available Qlik Talend Studio releases")
	downloadVer := flag.String("download", "", "Specify a release folder to download and zip (e.g. R2026-05)")
	workerFlag := flag.Int("workers", defaultWorkers, "Number of concurrent download workers")
	versionFlag := flag.Bool("version", false, "Print the current version of this application")
	flag.Parse()

	// Handle version output
	if *versionFlag {
		fmt.Printf("Qlik Talend Studio Release Packager CLI v%s\n", AppVersion)
		return
	}

	// Handle version listing
	if *listFlag {
		fmt.Printf("Qlik Talend Studio Release Packager CLI v%s\n", AppVersion)
		listReleases()
		return
	}

	// Handle execution pipeline
	if *downloadVer != "" {
		fmt.Printf("Qlik Talend Studio Release Packager CLI v%s\n", AppVersion)
		downloadAndZipRelease(*downloadVer, *workerFlag)
		return
	}

	// Default fallback help
	fmt.Printf("Qlik Talend Studio Release Packager CLI v%s\n\n", AppVersion)
	fmt.Println("Usage:")
	fmt.Println("  -list                 List available releases in the repository")
	fmt.Println("  -download <release>   Download and zip a specific release (e.g. R2026-05)")
	fmt.Println("  -workers <num>        Override the number of concurrent download threads")
	fmt.Println("  -version              Show app version details")
	fmt.Println("\nFlags configuration:")
	flag.PrintDefaults()
}

// 1. LIST ALL AVAILABLE RELEASES
func listReleases() {
	fmt.Println("Fetching available releases from repository...")
	resp, err := http.Get(baseURL)
	if err != nil {
		fmt.Printf("Error connecting to server: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		fmt.Printf("Failed to fetch releases. HTTP Status: %d\n", resp.StatusCode)
		return
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		fmt.Printf("Error parsing HTML payload: %v\n", err)
		return
	}

	var releases []string
	doc.Find("tr").Each(func(i int, s *goquery.Selection) {
		alt, _ := s.Find("img").Attr("alt")
		if alt == "[DIR]" {
			href, _ := s.Find("a").Attr("href")
			release := strings.TrimSuffix(href, "/")
			if release != "" && !strings.HasPrefix(release, "..") {
				releases = append(releases, release)
			}
		}
	})

	fmt.Println("\nAvailable Releases (Latest first):")
	fmt.Println("---------------------------------")
	for i := len(releases) - 1; i >= 0; i-- {
		fmt.Printf(" * %s\n", releases[i])
	}
}

// 2. DOWNLOAD AND ZIP A SPECIFIC RELEASE
func downloadAndZipRelease(release string, concurrencyLimit int) {
	releaseURL := fmt.Sprintf("%s%s/", baseURL, release)
	fmt.Printf("Scanning repository directories for release %s...\n", release)

	var tasks []Task
	err := discoverFiles(releaseURL, "", &tasks)
	if err != nil {
		fmt.Printf("Error discovering workspace files: %v\n", err)
		return
	}

	totalFiles = int64(len(tasks))
	if totalFiles == 0 {
		fmt.Println("No target files found. Validate the input release patch descriptor syntax.")
		return
	}
	fmt.Printf("Found %d target files to download.\n", totalFiles)

	outputDir := filepath.Join(".", outputDir)
	if err := os.MkdirAll(outputDir, os.ModePerm); err != nil {
		fmt.Printf("Failed creating target directory footprint: %v\n", err)
		return
	}

	zipFilePath := filepath.Join(outputDir, fmt.Sprintf("%s.zip", release))
	zipFile, err := os.Create(zipFilePath)
	if err != nil {
		fmt.Printf("Failed creating target file write stream: %v\n", err)
		return
	}
	defer zipFile.Close()

	// Level 0 configuration matches maximum performance execution profiles
	zipWriter := zip.NewWriter(zipFile)
	defer zipWriter.Close()

	taskChan := make(chan Task, totalFiles)
	var wg sync.WaitGroup
	var zipMutex sync.Mutex

	startTime = time.Now()
	fmt.Printf("Starting execution pipeline (Workers: %d | Host Cores: %d)...\n\n", concurrencyLimit, runtime.NumCPU())

	// Launch exact configured worker allocations
	for w := 1; w <= concurrencyLimit; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range taskChan {
				downloadWorker(task, zipWriter, &zipMutex)
			}
		}()
	}

	for _, task := range tasks {
		taskChan <- task
	}
	close(taskChan)

	wg.Wait()

	elapsedSeconds := time.Since(startTime).Seconds()
	fmt.Println("\n---------------------------------------------------------------------------------")
	fmt.Printf("Success! Release %s processed completely.\n", release)
	fmt.Printf("Target Asset Archive Location: %s\n", zipFilePath)
	fmt.Printf("Total files downloaded: %d/%d\n", atomic.LoadInt64(&filesDownloaded), totalFiles)
	fmt.Printf("Total bytes processed: %.2f MB\n", float64(atomic.LoadInt64(&totalBytesDownloaded))/1000000.0)
	fmt.Printf("Total runtime duration: %.2f s\n", elapsedSeconds)
}

// Recursive directory structural scraper
func discoverFiles(currentURL, currentRelPath string, tasks *[]Task) error {
	resp, err := http.Get(currentURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d when accessing web target: %s", resp.StatusCode, currentURL)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return err
	}

	var discoveryErr error
	doc.Find("tr").Each(func(i int, s *goquery.Selection) {
		alt, _ := s.Find("img").Attr("alt")
		link := s.Find("a")
		href, exists := link.Attr("href")

		if !exists || strings.HasPrefix(href, "?") || strings.HasPrefix(href, "/") {
			return
		}

		parsedBase, _ := url.Parse(currentURL)
		parsedHref, _ := url.Parse(href)
		fullURL := parsedBase.ResolveReference(parsedHref).String()

		switch alt {
		case "[   ]", "[TXT]":
			*tasks = append(*tasks, Task{
				URL:     fullURL,
				ZipPath: filepath.ToSlash(filepath.Join(currentRelPath, href)),
			})
		case "[DIR]":
			subDirURL := fullURL
			subRelPath := filepath.Join(currentRelPath, href)
			err := discoverFiles(subDirURL, subRelPath, tasks)
			if err != nil {
				discoveryErr = err
			}
		}
	})

	return discoveryErr
}

// Worker logic supervising streaming connections with exponential backoff
func downloadWorker(task Task, zipWriter *zip.Writer, mu *sync.Mutex) {
	var bytesCopied int64
	var err error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		bytesCopied, err = streamToZipAttempt(task, zipWriter, mu)
		if err == nil {
			break // Success!
		}

		if attempt < maxRetries {
			// Calculate Exponential Backoff: baseWait * (2 ^ (attempt - 1))
			// Attempt 1: 2 * 1 = 2s | Attempt 2: 2 * 2 = 4s | Attempt 3: 2 * 4 = 8s | Attempt 4: 16s...
			waitTime := time.Duration(baseWait*math.Pow(2, float64(attempt-1))) * time.Second

			// Cap the maximum backoff to 30 seconds so it doesn't sleep indefinitely
			if waitTime > 30*time.Second {
				waitTime = 30 * time.Second
			}

			fmt.Printf("[WARNING] Attempt %d failed for %s (%v). Backing off for %v...\n",
				attempt, filepath.Base(task.ZipPath), err, waitTime)

			time.Sleep(waitTime)
		} else {
			fmt.Printf("[ERROR] Persistent communication drop encountered on %s after %d attempts: %v\n",
				filepath.Base(task.ZipPath), maxRetries, err)
			return
		}
	}

	// Update telemetry stats
	atomic.AddInt64(&totalBytesDownloaded, bytesCopied)
	currentDoneFiles := atomic.AddInt64(&filesDownloaded, 1)

	totalElapsed := time.Since(startTime).Seconds()
	if totalElapsed <= 0 {
		totalElapsed = 0.001
	}
	avgSpeedMBps := (float64(atomic.LoadInt64(&totalBytesDownloaded)) / 1000000.0) / totalElapsed

	fmt.Printf("[%d/%d] Streamed: %s | %.3f MB | Current Avg: %.2f MB/s\n",
		currentDoneFiles, atomic.LoadInt64(&totalFiles), filepath.Base(task.ZipPath), float64(bytesCopied)/1000000.0, avgSpeedMBps)
}

// Low-level function handling the atomic network connection and writing
func streamToZipAttempt(task Task, zipWriter *zip.Writer, mu *sync.Mutex) (int64, error) {
	// Create a custom client with a generous timeout to prevent premature drops on large files
	client := &http.Client{
		Timeout: 5 * time.Minute, // Give large files up to 5 minutes to finish streaming
	}

	resp, err := client.Get(task.URL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Acquire lock BEFORE we touch the zip archive structure
	mu.Lock()
	writer, err := zipWriter.Create(task.ZipPath)
	if err != nil {
		mu.Unlock()
		return 0, fmt.Errorf("zip structural mutation failure: %w", err)
	}

	// Stream data over the wire directly into the ZIP writer
	bytesWritten, err := io.Copy(writer, resp.Body)
	mu.Unlock() // Always release the lock immediately when done

	if err != nil {
		return 0, err
	}

	return bytesWritten, nil
}
