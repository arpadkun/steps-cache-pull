package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bitrise-io/go-steputils/stepconf"
	"github.com/bitrise-io/go-utils/log"
)

// Config stores the step inputs.
type Config struct {
	CacheAPIURL string `env:"cache_api_url"`
	DebugMode   bool   `env:"is_debug_mode,opt[true,false]"`
	StackID     string `env:"BITRISEIO_STACK_ID"`
}

// downloadCacheArchive downloads the cache archive and returns the downloaded file's path.
// If the URI points to a local file it returns the local paths.
func downloadCacheArchive(url string) (string, error) {
	if strings.HasPrefix(url, "file://") {
		return strings.TrimPrefix(url, "file://"), nil
	}

	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Warnf("Failed to close response body: %s", err)
		}
	}()

	if resp.StatusCode != 200 {
		responseBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}

		return "", fmt.Errorf("non success response code: %d, body: %s", resp.StatusCode, string(responseBytes))
	}

	const cacheArchivePath = "/tmp/cache-archive.tar"
	f, err := os.Create(cacheArchivePath)
	if err != nil {
		return "", fmt.Errorf("failed to open the local cache file for write: %s", err)
	}

	_, err = io.Copy(f, resp.Body)
	if err != nil {
		return "", err
	}

	return cacheArchivePath, nil
}

// performRequest performs an http request and returns the response's body, if the status code is 200.
func performRequest(url string) (io.ReadCloser, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		defer func() {
			if err := resp.Body.Close(); err != nil {
				log.Warnf("Failed to close response body: %s", err)
			}
		}()

		responseBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		return nil, fmt.Errorf("non success response code: %d, body: %s", resp.StatusCode, string(responseBytes))
	}

	return resp.Body, nil
}

// getCacheDownloadURL gets the given build's cache download URL.
func getCacheDownloadURL(cacheAPIURL string) (string, error) {
	req, err := http.NewRequest("GET", cacheAPIURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %s", err)
	}

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %s", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Warnf("Failed to close response body: %s", err)
		}
	}()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("request sent, but failed to read response body (http-code: %d): %s", resp.StatusCode, body)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 202 {
		return "", fmt.Errorf("build cache not found: probably cache not initialised yet (first cache push initialises the cache), nothing to worry about ;)")
	}

	var respModel struct {
		DownloadURL string `json:"download_url"`
	}
	if err := json.Unmarshal(body, &respModel); err != nil {
		return "", fmt.Errorf("failed to parse JSON response (%s): %s", body, err)
	}

	if respModel.DownloadURL == "" {
		return "", errors.New("download URL not included in the response")
	}

	return respModel.DownloadURL, nil
}

// parseStackID reads the stack id from the given json bytes.
func parseStackID(b []byte) (string, error) {
	type ArchiveInfo struct {
		StackID string `json:"stack_id,omitempty"`
	}
	var archiveInfo ArchiveInfo
	if err := json.Unmarshal(b, &archiveInfo); err != nil {
		return "", err
	}
	return archiveInfo.StackID, nil
}

// failf prints an error and terminates the step.
func failf(format string, args ...interface{}) {
	log.Errorf(format, args...)
	os.Exit(1)
}

func main() {
	var conf Config
	if err := stepconf.Parse(&conf); err != nil {
		failf(err.Error())
	}
	stepconf.Print(conf)
	log.SetEnableDebugLog(conf.DebugMode)

	// if conf.CacheAPIURL == "" {
	// 	log.Warnf("No Cache API URL specified, there's no cache to use, exiting.")
	// 	return
	// }

	startTime := time.Now()

	//var cacheReader io.Reader
	//var cacheURI string

	// if strings.HasPrefix(conf.CacheAPIURL, "file://") {
	// 	cacheURI = conf.CacheAPIURL

	// 	fmt.Println()
	// 	log.Infof("Using local cache archive")

	// 	pth := strings.TrimPrefix(conf.CacheAPIURL, "file://")

	// 	var err error
	// 	cacheReader, err = os.Open(pth)
	// 	if err != nil {
	// 		failf("Failed to open cache archive file: %s", err)
	// 	}
	// } else {
	// 	fmt.Println()
	// 	log.Infof("Downloading remote cache archive")

	// 	downloadURL, err := getCacheDownloadURL(conf.CacheAPIURL)
	// 	if err != nil {
	// 		failf("Failed to get cache download url: %s", err)
	// 	}
	// 	cacheURI = downloadURL

	// 	cacheReader, err = performRequest(downloadURL)
	// 	if err != nil {
	// 		failf("Failed to perform cache download request: %s", err)
	// 	}
	// }

	// cacheRecorderReader := NewRestoreReader(cacheReader)

	// currentStackID := strings.TrimSpace(conf.StackID)
	// if len(currentStackID) > 0 {
	// 	fmt.Println()
	// 	log.Infof("Checking archive and current stacks")
	// 	log.Printf("current stack id: %s", currentStackID)

	// 	r, hdr, err := readFirstEntry(cacheRecorderReader)
	// 	if err != nil {
	// 		failf("Failed to get first archive entry: %s", err)
	// 	}

	// 	cacheRecorderReader.Restore()

	// 	if filepath.Base(hdr.Name) == "archive_info.json" {
	// 		b, err := ioutil.ReadAll(r)
	// 		if err != nil {
	// 			failf("Failed to read first archive entry: %s", err)
	// 		}

	// 		archiveStackID, err := parseStackID(b)
	// 		if err != nil {
	// 			failf("Failed to parse first archive entry: %s", err)
	// 		}
	// 		log.Printf("archive stack id: %s", archiveStackID)

	// 		if archiveStackID != currentStackID {
	// 			log.Warnf("Cache was created on stack: %s, current stack: %s", archiveStackID, currentStackID)
	// 			log.Warnf("Skipping cache pull, because of the stack has changed")
	// 			os.Exit(0)
	// 		}
	// 	} else {
	// 		log.Warnf("cache archive does not contain stack information, skipping stack check")
	// 	}
	// }

	fmt.Println()
	log.Infof("Extracting cache archive")

	// if err := extractCacheArchive(cacheRecorderReader); err != nil {
	// 	log.Warnf("Failed to uncompress cache archive stream: %s", err)
	// 	log.Warnf("Downloading the archive file and trying to uncompress using tar tool")

	// 	pth, err := downloadCacheArchive(cacheURI)
	// 	if err != nil {
	// 		failf("Fallback failed, unable to download cache archive: %s", err)
	// 	}

	// 	if err := uncompressArchive(pth); err != nil {
	// 		failf("Fallback failed, unable to uncompress cache archive file: %s", err)
	// 	}
	// }

	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

	log.Printf("-----------------------------AKARMI Printf-----------------------------")
	log.Infof("-----------------------------AKARMI Infof-----------------------------")
	fmt.Println("-----------------------------AKARMI fmt.println-----------------------------")

	// Requirements, set up ENV Variables as secrets <-----:
	// LOCAL_CACHE_DST_URL
	// LOCAL_CACHE_KEY

	// Getting the ssh key into variable
	LocalCacheKey := os.Getenv("LOCAL_CACHE_KEY")
	LocalCacheKeyDecoded, _ := base64.URLEncoding.DecodeString(LocalCacheKey)

	// Write the ssh key to file
	HomeDir := os.Getenv("HOME")
	LocalCacheStorageSSHKeyFile := HomeDir + "/.ssh/local_cache.key"
	LocalCacheFilesListFile := HomeDir + "/.local_cache_file_list"
	LocalCacheFilesDstURL := os.Getenv("LOCAL_CACHE_DST_URL")
	LocalCacheStoragePort := "22"
	LocalCacheStoragePortTimeout := "3"

	// Write ssh key file
	file, err := os.Create(LocalCacheStorageSSHKeyFile)
	if err != nil {
		fmt.Println(err)
	} else {
		file.WriteString(string(LocalCacheKeyDecoded))
	}
	file.Close()

	err = os.Chmod(LocalCacheStorageSSHKeyFile, 0600)
	if err != nil {
		fmt.Println(err)
	}

	//////////////////////////////////////////
	// Configuring rsync parameters
	rsyncSettingsSSHsetup := "/usr/bin/ssh -i " + LocalCacheStorageSSHKeyFile + " -o ConnectTimeout=" + LocalCacheStoragePortTimeout + " -p " + LocalCacheStoragePort
	rsyncSettingsFilesFrom := "--files-from=" + LocalCacheFilesListFile
	rsyncSettingsDestinationURL := LocalCacheFilesDstURL

	// Downloading file list first
	log.Infof("Downloading file list first...")
	rsyncArgsListOnly := []string{"-e", rsyncSettingsSSHsetup, "--dirs", "--archive", "--no-D", "--inplace", "--executability", "--ignore-errors", "--force", "--compress", "--stats", "--human-readable", "--no-whole-file", "--prune-empty-dirs", rsyncSettingsDestinationURL + LocalCacheFilesListFile, HomeDir}
	fmt.Printf("%v\n\n", rsyncArgsListOnly)

	cmdGetList := exec.Command("rsync", rsyncArgsListOnly...)

	outputGetList, err := cmdGetList.CombinedOutput()
	if err != nil {
		os.Stderr.WriteString(fmt.Sprintf("==> Error: %v\n", err.Error()))
	}
	log.Printf("%v\n", string(outputGetList))

	// Syncing down the files using the downloaded list
	log.Infof("Syncing files now...")
	rsyncArgs := []string{"-e", rsyncSettingsSSHsetup, rsyncSettingsFilesFrom, "--dirs", "--relative", "--archive", "--no-D", "--inplace", "--executability", "--delete", "--ignore-errors", "--force", "--compress", "--stats", "--human-readable", "--no-whole-file", "--prune-empty-dirs", rsyncSettingsDestinationURL, "/"}
	fmt.Printf("%v\n\n", rsyncArgs)

	cmd := exec.Command("rsync", rsyncArgs...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		os.Stderr.WriteString(fmt.Sprintf("==> Error: %v\n", err.Error()))
	}
	log.Printf("%v\n", string(output))

	// K:  cycling through the files/directories that are required to be saved
	// log.Printf("\n============================================================================================")
	// for pth := range pathToIndicatorPath {
	// 	// log.Printf("%s", pth)
	// 	// log.Printf("============================================================================================")
	// 	// log.Printf("This is in the pathToIndicatorPath variable: %s", pth)
	// 	var cmd1 = exec.Command("file", pth)
	// 	output, err := cmd1.Output()
	// 	if err != nil {
	// 		log.Printf("Could not run find, failed")
	// 	}
	// 	log.Printf("%v\n", string(output))

	// 	// // If the path is directory, let's print the contents
	// 	// if info, err := os.Stat(pth); err == nil && info.IsDir() {
	// 	// 	var cmd2 = exec.Command("find", pth)
	// 	// 	output, err := cmd2.Output()
	// 	// 	if err != nil {
	// 	// 		log.Printf("Could not run find, failed")
	// 	// 	}
	// 	// 	log.Printf("------------ Directory [%s] Contents ------------:\n  %v\n", pth, string(output))
	// 	// }
	// }

	log.Printf("============================================================================================")

	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

	fmt.Println()
	log.Donef("Done")
	log.Printf("Took: " + time.Since(startTime).String())
}
