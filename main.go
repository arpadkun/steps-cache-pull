package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
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

/////////////////////////////////////////////
// rsync process' function
func rsyncProcess(receivedRsyncParams []string) (string, error) {

	rsynccmd := exec.Command("time", receivedRsyncParams...)

	var output string
	rsyncoutput, err := rsynccmd.CombinedOutput()
	if err != nil {
		output = string(rsyncoutput)
		return output, err
	}

	output = string(rsyncoutput)
	return output, nil
}

// Worker for parallel processing (running rsync on multiple threads)
func worker(id int, LocalCacheFilesListFile string, FilesForOneWorker []string, wg *sync.WaitGroup) {

	defer wg.Done()

	fmt.Printf("\n------------------------\nWorker %d starting\n", id)
	LocalCacheFilesListFileWorkerID := LocalCacheFilesListFile + "." + strconv.Itoa(id)
	fmt.Printf("Writing chunk for worker %d to: %v\n", id, LocalCacheFilesListFileWorkerID)

	// Write file list chunk to temporary sync to LocalCacheFilesListFile.worker#
	filesListFile, err := os.Create(LocalCacheFilesListFileWorkerID)
	if err != nil {
		fmt.Println(err)
	} else {
		for _, pth := range FilesForOneWorker {
			filesListFile.WriteString(string(pth) + "\n")
		}
	}
	filesListFile.Close()

	// NEED TO FIX -- restructure to remove these variables, testing only...
	HomeDir := os.Getenv("HOME")
	LocalCacheStorageSSHKeyFile := HomeDir + "/.ssh/local_cache.key"
	LocalCacheFilesDstURL := os.Getenv("LOCAL_CACHE_DST_URL")
	LocalCacheStoragePort := "22"
	LocalCacheStoragePortTimeout := "3"

	rsyncSettingsSSHsetup := "/usr/bin/ssh -i " + LocalCacheStorageSSHKeyFile + " -o ConnectTimeout=" + LocalCacheStoragePortTimeout + " -p " + LocalCacheStoragePort
	rsyncSettingsFilesFrom := "--files-from=" + LocalCacheFilesListFileWorkerID
	rsyncSettingsDestinationURL := LocalCacheFilesDstURL

	rsyncArgs := []string{"rsync", "-e", rsyncSettingsSSHsetup, rsyncSettingsFilesFrom, "--relative", "--archive", "--no-D", "--executability", "--delete", "--ignore-errors", "--force", "--stats", "--human-readable", rsyncSettingsDestinationURL, "/"} // "--compress", "--dirs", "--inplace","--no-whole-file",
	fmt.Printf("DEBUG:  %v\n\n", rsyncArgs)

	// Starting the rsync process here
	output, err := rsyncProcess(rsyncArgs)
	if err != nil {
		os.Stderr.WriteString(fmt.Sprintf("==> rsync error:\n%v\n\n", err.Error()))
	}
	log.Printf("==> rsync output:\n%v\n\n", string(output))

	//fmt.Printf("#v:  %#v\n\n", FilesForOneWorker)
	//time.Sleep(time.Second)
	fmt.Printf("Worker %d done\n", id)
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

	// Requirements, set up ENV Variables as secrets <-----:
	// LOCAL_CACHE_DST_URL
	// LOCAL_CACHE_KEY

	// Getting the ssh key into variable
	if len(os.Getenv("LOCAL_CACHE_KEY")) < 1000 {
		failf("ERROR: missing or invalid required environment variable:  LOCAL_CACHE_KEY")
	}

	if len(os.Getenv("LOCAL_CACHE_DST_URL")) < 15 {
		failf("ERROR: missing or invalid required environment variable:  LOCAL_CACHE_DST_URL")
	}

	LocalCacheKey := os.Getenv("LOCAL_CACHE_KEY")
	LocalCacheKeyDecoded, _ := base64.URLEncoding.DecodeString(LocalCacheKey)

	HomeDir := os.Getenv("HOME")
	LocalCacheStorageSSHKeyFile := HomeDir + "/.ssh/local_cache.key"

	LocalCacheFilesListDir := HomeDir + "/.local_cache_xfer_lists"                      // Dir - file lists go into this directory (control plane)
	LocalCacheFilesListFile := LocalCacheFilesListDir + "/local_cache_file_list"        // File - list of all files to transfer
	LocalCacheLargeFilesDirXferList := LocalCacheFilesListDir + "/chunked_dir_list"     // File - list containing which directories were tar'ed and chunked up
	LocalCacheLargeFilesDirXferChunks := LocalCacheFilesListDir + "/chunked_dir_chunks" // File - list containing the split tar file names, like xaa, xab, etc... Using this to send up what needs to be pulled
	LocalCacheLargeFilesDirXferDir := HomeDir + "/.local_cache_xfer"                    // Dir - for large file chunks that are split up (data plane)

	LocalCacheFilesDstURL := os.Getenv("LOCAL_CACHE_DST_URL")
	LocalCacheStoragePort := "22"
	LocalCacheStoragePortTimeout := "3"

	// Write the ssh key to file
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

	var numCPU int
	if len(os.Getenv("LOCAL_CACHE_SYNC_WORKERS")) == 0 {
		numCPU = 6 // Default for parallel workers if ENV variable is missing
	} else {
		numCPU, err = strconv.Atoi(os.Getenv("LOCAL_CACHE_SYNC_WORKERS"))
	}

	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Downloading the file containing what files to sync (a file with a list of of files and directories)
	// Configuring rsync parameters
	rsyncSettingsSSHsetup := "/usr/bin/ssh -i " + LocalCacheStorageSSHKeyFile + " -o ConnectTimeout=" + LocalCacheStoragePortTimeout + " -p " + LocalCacheStoragePort
	//rsyncSettingsFilesFrom := "--files-from=" + LocalCacheFilesListFile
	rsyncSettingsDestinationURL := LocalCacheFilesDstURL

	// Downloading two file lists first: the file with all files to transfer and the list of the chunked directories
	log.Infof("Downloading file list first...")
	rsyncArgsListOnly := []string{"rsync", "-e", rsyncSettingsSSHsetup, "--dirs", "--archive", "--no-D", "--inplace", "--executability", "--ignore-errors", "--force", "--stats", "--human-readable", "--no-whole-file", rsyncSettingsDestinationURL + LocalCacheFilesListDir, HomeDir} // "--compress",

	fmt.Printf("DEBUG:  %v\n\n", rsyncArgsListOnly)

	rsyncoutput, err := rsyncProcess(rsyncArgsListOnly)
	if err != nil {
		os.Stderr.WriteString(fmt.Sprintf("==> rsync error:\n%v\n\n", err.Error()))
	}

	fmt.Printf("==> rsync output:\n%v\n\n", rsyncoutput)

	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// Syncing down the files using the downloaded list on multiple threads

	// Reading file list and splitting it up into arrays:  [worker#][array of file names]

	filesToSyncSourceFile, err := os.Open(LocalCacheFilesListFile)
	if err != nil {
		log.Warnf("Failed to open list of files to sync, assuming no previous cache, continuing without pulling cache: %s", err)
		return
	}

	scanner := bufio.NewScanner(filesToSyncSourceFile)
	scanner.Split(bufio.ScanLines)
	var FilesToSync []string

	for scanner.Scan() {
		FilesToSync = append(FilesToSync, scanner.Text())
	}
	filesToSyncSourceFile.Close()

	/////////////////////////////////////////////////////////////////////////////////////////
	// Split up the list ~equally for parallel processing, but only if there are more than a 100 items:

	numberOfFiles := len(FilesToSync)
	if numberOfFiles == 0 {
		log.Infof("File list is empty, nothing to sync")
		return
	}

	if numberOfFiles < 100 {
		numCPU = 1
	}

	fmt.Printf("Number of files to sync: %#v\n", numberOfFiles)
	fmt.Printf("Workers to spawn: %#v\n", numCPU)

	var filesDivided [][]string
	chunkSize := (len(FilesToSync) + numCPU - 1) / numCPU
	fmt.Printf("Chunksize per worker: %#v\n", chunkSize)

	sort.Strings(FilesToSync)
	for i := 0; i < len(FilesToSync); i += chunkSize {
		end := i + chunkSize
		if end > len(FilesToSync) {
			end = len(FilesToSync)
		}

		filesDivided = append(filesDivided, FilesToSync[i:end])

	}

	/////////////////////////////////////////////////////////////////////////////////////////
	// Load up chunks file names (xaa, xab, xac) from LocalCacheLargeFilesDirXferChunks and distribute them across workers:
	var xferChunkFileList []string
	chunksSourceFile, err := os.Open(LocalCacheLargeFilesDirXferChunks)
	if err != nil {
		log.Warnf("Failed to open *chunk list* file, assuming no \"Directory with large files\" was configured (this is OK), continuing: %s", err)
	} else {
		scanner := bufio.NewScanner(chunksSourceFile)
		scanner.Split(bufio.ScanLines)

		for scanner.Scan() {
			xferChunkFileList = append(xferChunkFileList, scanner.Text())
		}
		chunksSourceFile.Close()
	}

	// Distributing to workwer arrays based on worker count
	var numCPUCounter int
	numCPUCounter = 0
	numOfChunkedFiles := len(xferChunkFileList)
	fmt.Printf("Number of Chunked Files:  %v\n", numOfChunkedFiles)
	for e := 0; e <= numOfChunkedFiles-1; e++ { // Cycling through all files in the chunk list
		fmt.Printf("Worker %v gets chunkfile:  %v (%v/%v)\n", numCPUCounter, xferChunkFileList[e], e+1, numOfChunkedFiles)

		filesDivided[numCPUCounter] = append(filesDivided[numCPUCounter], xferChunkFileList[e]) // Distributing chunk files into the filesDivided arrays
		if numCPUCounter == numCPU-1 {
			numCPUCounter = -1 // Once reached the max numCPU count, reset to the first array
		}
		numCPUCounter++
	}

	/////////////////////////////////////////////////////////////////////////////////////////
	// Spin up workers
	log.Infof("Syncing files now...")

	var wg sync.WaitGroup

	for i := 0; i <= numCPU-1; i++ {
		wg.Add(1)

		go worker(i, LocalCacheFilesListFile, filesDivided[i], &wg)
	}

	wg.Wait()

	/////////////////////////////////////////////////////////////////////////////////////////
	// Once all files arrived, let's unpack the chunked directories

	readChunkedDirsToUnpack, err := os.Open(LocalCacheLargeFilesDirXferList)

	if err != nil {
		log.Warnf("Failed to open list directories with large files, assuming none was created on push step, continuing: %s", err)
		return
	}

	chunkedscanner := bufio.NewScanner(readChunkedDirsToUnpack)
	chunkedscanner.Split(bufio.ScanLines)
	var chunkedDirsToUnpack []string

	for chunkedscanner.Scan() {
		chunkedDirsToUnpack = append(chunkedDirsToUnpack, chunkedscanner.Text())
	}
	readChunkedDirsToUnpack.Close()

	numberOfChunkedDirs := len(chunkedDirsToUnpack)
	if numberOfChunkedDirs == 0 {
		log.Infof("File list for directories with large files is empty, nothing to unpack, continuing...")
		return
	}

	fmt.Printf("Number of files to sync: %#v\n", numberOfChunkedDirs)

	for unpackThisDir := range chunkedDirsToUnpack {

		chunkFiles := LocalCacheLargeFilesDirXferDir + chunkedDirsToUnpack[unpackThisDir] + "/x*"
		fmt.Printf("Unpacking Directory: [%s] from [%s] ...\n", chunkedDirsToUnpack[unpackThisDir], chunkFiles)

		// Concatenating the chunks into tar and extracting
		// cat /Users/vagrant/.local_cache_xfer/Users/vagrant/git/.git/x* | tar -xf - -C /
		// tarDirArgs := []string{"cat", "-lcf", archiveName, packThisDir}

		untarCmd := "cat " + chunkFiles + " | tar -xf - -C /"
		out, err := exec.Command("bash", "-c", untarCmd).CombinedOutput()
		if err != nil {
			os.Stderr.WriteString(fmt.Sprintf("==> tar unpacking error:\n%v\n\n", err.Error()))
		}
		log.Printf("%v\n", string(out))

		// if err != nil {
		// 	fmt.Sprintf("Failed to execute command: %s", string(untarCmd))
		// }
		// fmt.Println(string(out))

		///////////////////////////////////////////////////////////////
		// Could not make the builtin pipe work
		// catCmd := exec.Command("cat", chunkFiles)
		// untarCmd := exec.Command("tar", "-xf", "-", "-C", "/")

		// pr, pw := io.Pipe()
		// catCmd.Stdout = pw
		// untarCmd.Stdin = pr
		// untarCmd.Stdout = os.Stdout

		// catCmd.Start()
		// untarCmd.Start()

		// go func() {
		// 	defer pw.Close()
		// 	catCmd.Wait()
		// }()
		// untarCmd.Wait()
		///////////////////////////////////////////////////////////////
	}

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
