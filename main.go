package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ArneVogel/concat/vod"
	"golang.org/x/crypto/ssh/terminal"
)

//new style of edgecast links: http://vod089-ttvnw.akamaized.net/1059582120fbff1a392a_reinierboortman_26420932624_719978480/chunked/highlight-180380104.m3u8
//old style of edgecast links: http://vod164-ttvnw.akamaized.net/7a16586e4b7ef40300ba_zizaran_27258736688_772341213/chunked/index-dvr.m3u8

const edgecastLinkBegin string = "http://"
const edgecastLinkBaseEndOld string = "index"
const edgecastLinkBaseEnd string = "highlight"
const edgecastLinkM3U8End string = ".m3u8"
const targetdurationStart string = "TARGETDURATION:"
const targetdurationEnd string = "\n#ID3"
const sourceQuality string = "chunked"
const chunkFileExtension string = ".ts"
const currentReleaseLink string = "https://github.com/ArneVogel/concat/releases/latest"
const currentReleaseStart string = `<a href="/ArneVogel/concat/releases/download/`
const currentReleaseEnd string = `/concat"`
const versionNumber string = "v0.2.2"

var ffmpegCMD = `ffmpeg`

var debug bool
var maximumConcurrency int
var useVideoTitle bool
var noProgress bool

var m sync.Once
var ctxt, ctxtAbortFn = context.WithCancel(context.Background())
var closed = false
var cleanUpQueue = make([]func(), 0)
var done = make(chan struct{}, 1)
var wg sync.WaitGroup
var httpClient = &http.Client{Timeout: time.Minute}

var totalChunks int
var chunksCompleted = 0

/*
	Returns the number of chunks to download based of the start and end time and the target duration of a
	chunk. Adding 1 to overshoot the end by a bit
*/
func numberOfChunks(sh int, sm int, ss int, eh int, em int, es int, target int) int {
	startSeconds := sh*3600 + sm*60 + ss
	endSeconds := eh*3600 + em*60 + es

	return ((endSeconds - startSeconds) / target) + 1
}

func startingChunk(sh int, sm int, ss int, target int) int {
	startSeconds := sh*3600 + sm*60 + ss
	return (startSeconds / target)
}

func downloadChunk(newpath string, edgecastBaseURL string, chunkNum string, chunkName string, vodID string) error {
	if debug {
		fmt.Printf("Downloading: %s\n", edgecastBaseURL+chunkName)
	}
	req, err := http.NewRequest("GET", edgecastBaseURL+chunkName, nil)
	if err != nil {
		return fmt.Errorf("Could not reach %s: '%v'", edgecastBaseURL+chunkName, err)
	}
	req = req.WithContext(ctxt)
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Could not get '%v'", err)
	}

	resultFile, err := os.Create(filepath.Join(newpath, vodID+"_"+chunkNum+chunkFileExtension))
	if err != nil {
		return fmt.Errorf("Could not create file '%s'", newpath+"/"+vodID+"_"+chunkNum+chunkFileExtension)
	}
	defer resultFile.Close()
	if _, err := io.Copy(resultFile, resp.Body); err != nil {
		return fmt.Errorf("Could not download file '%s'. %v", vodID+"_"+chunkNum+chunkFileExtension, err)
	}
	chunksCompleted++
	if !debug && !noProgress {
		w, _, err := terminal.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			w = 50
		} else {
			w -= 8 // Brackets + space + percentage-Value + percentage-sign + 1
		}
		paddingSize := w
		percentage := float32(chunksCompleted) / float32(totalChunks)
		pWidth := (float32(chunksCompleted) / float32(totalChunks)) * float32(paddingSize)
		fmt.Printf("\r[%s%s] %3.0f%%", strings.Repeat("█", int(pWidth)), strings.Repeat("░", paddingSize-int(pWidth)), percentage*100)
	}
	return nil
}

func createConcatFile(newpath string, chunkNum int, startChunk int, v vod.Vod) (*os.File, error) {
	tempFile, err := ioutil.TempFile(newpath, "concat_"+v.ID)
	if err != nil {
		return nil, err
	}
	defer tempFile.Close()
	concatBuf := bytes.NewBuffer(make([]byte, 0))
	for i := startChunk; i < (startChunk + chunkNum); i++ {
		s := strconv.Itoa(i)
		concatBuf.WriteString("file '")
		filePath, _ := filepath.Abs(newpath + "/" + v.ID + "_" + s + chunkFileExtension)
		concatBuf.WriteString(filePath)
		concatBuf.WriteRune('\'')
		concatBuf.WriteRune('\n')
	}
	concat := concatBuf.String()
	if _, err := tempFile.WriteString(concat); err != nil {
		return nil, err
	}
	return tempFile, nil
}

func sanitizeFilename(fileName string) string {
	invalidLetters := [...]string{`?`, `\`, `/`, `:`, `*`, `>`, `<`, `|`, "\x00"}
	sanitized := fileName
	for _, l := range invalidLetters {
		sanitized = strings.Replace(sanitized, l, "", -1)
	}
	return sanitized
}
func ffmpegCombine(newpath string, chunkNum int, startChunk int, v vod.Vod) {
	tempFile, err := createConcatFile(newpath, chunkNum, startChunk, v)
	if err != nil {
		fmt.Println(err)
		return
	}
	cleanUpQueue = append(cleanUpQueue, func() {
		os.Remove(tempFile.Name())
	})

	videoName := v.ID
	if useVideoTitle && v.Title != "" {
		videoName = fmt.Sprintf(`%s_%s`, sanitizeFilename(v.Title), v.ID)
	}

	args := []string{"-f", "concat", "-safe", "0", "-i", tempFile.Name(), "-c", "copy", "-bsf:a", "aac_adtstoasc", "-fflags", "+genpts", fmt.Sprintf(`%s.mp4`, videoName)}

	if debug {
		fmt.Printf("Running ffmpeg: %s %s\n", ffmpegCMD, args)
	}

	cmd := exec.Command(ffmpegCMD, args...)
	var errbuf bytes.Buffer
	cmd.Stderr = &errbuf
	err = cmd.Run()
	if err != nil {
		fmt.Println(errbuf.String())
		fmt.Println("ffmpeg error")
	}
}

func deleteChunks(newpath string, chunkNum int, startChunk int, vodID string) {
	var del string
	for i := startChunk; i < (startChunk + chunkNum); i++ {
		s := strconv.Itoa(i)
		del = filepath.Join(newpath, vodID+"_"+s+chunkFileExtension)
		err := os.Remove(del)
		if err != nil && !os.IsNotExist(err) {
			fmt.Println("Could not delete all chunks, try manually deleting them", err)
		}
	}
}

func wrongInputNotification() {
	fmt.Println("Call the program with -help for information on how to use it :^)")
}

func downloadPartVOD(vodIDString string, start string, end string, quality string) {
	var vodSH, vodSM, vodSS, vodEH, vodEM, vodES int
	if end != "full" {
		startArray := strings.Split(start, " ")
		endArray := strings.Split(end, " ")

		vodSH, _ = strconv.Atoi(startArray[0]) //start Hour
		vodSM, _ = strconv.Atoi(startArray[1]) //start minute
		vodSS, _ = strconv.Atoi(startArray[2]) //start second
		vodEH, _ = strconv.Atoi(endArray[0])   //end hour
		vodEM, _ = strconv.Atoi(endArray[1])   //end minute
		vodES, _ = strconv.Atoi(endArray[2])   //end second

		if (vodSH*3600 + vodSM*60 + vodSS) > (vodEH*3600 + vodEM*60 + vodES) {
			fmt.Println("Start time is greater than end time!")
			wrongInputNotification()
		}
	}

	if !useVideoTitle {
		_, err := os.Stat(vodIDString + ".mp4")
		if err == nil || !os.IsNotExist(err) {
			fmt.Printf("Destination file \"%s\" already exists!\n", vodIDString+".mp4")
			abortWork()
			return
		}
	}

	vodStruct, err := vod.GetVod(vodIDString)
	if err != nil {
		fmt.Println(err)
		abortWork()
		return
	}

	if err := ctxt.Err(); err != nil {
		fmt.Println(err)
		return
	}

	if useVideoTitle {
		formatString := `%s_%s.mp4`
		if vodStruct.Title == "" { // If for some reason, the title could not be fetched
			formatString = `%s%s.mp4`
		}
		fileName := fmt.Sprintf(formatString, sanitizeFilename(vodStruct.Title), vodStruct.ID)
		_, err := os.Stat(fileName)
		if err == nil || !os.IsNotExist(err) {
			fmt.Printf("Destination file \"%s\" already exists!\n", fileName)
			abortWork()
			return
		}
	}

	edgecastURLmap := vodStruct.GetEdgecastURLMap()

	if debug {
		fmt.Println(edgecastURLmap)
	}

	// I don't see what this does. With this you can't download in source quality (chunked).
	// Fixed. But "chunked" playlist not always available, have to loop and find max quality manually

	m3u8Link, ok := edgecastURLmap[quality]

	if ok {
		fmt.Printf("Selected quality: %s\n", quality)
	} else {
		fmt.Printf("Couldn't find quality: %s\n", quality)

		// Try to find source quality playlist
		if quality != sourceQuality {
			quality = sourceQuality

			m3u8Link, ok = edgecastURLmap[quality]
		}

		if ok {
			fmt.Printf("Downloading in source quality: %s\n", quality)
		} else {
			// Quality still not matched
			resolutionMax := 0
			fpsMax := 0
			resolutionTmp := 0
			fpsTmp := 0
			var keyTmp []string

			// Find max quality
			for key := range edgecastURLmap {
				keyTmp = strings.Split(key, "p")

				resolutionTmp, _ = strconv.Atoi(keyTmp[0])

				if len(keyTmp) > 1 {
					fpsTmp, _ = strconv.Atoi(keyTmp[1])
				} else {
					fpsTmp = 0
				}

				if resolutionTmp > resolutionMax || resolutionTmp == resolutionMax && fpsTmp > fpsMax {
					quality = key
					fpsMax = fpsTmp
					resolutionMax = resolutionTmp
				}
			}

			m3u8Link, ok = edgecastURLmap[quality]

			if ok {
				fmt.Printf("Downloading in max available quality: %s\n", quality)
			} else {
				fmt.Println("No available quality options found")
				os.Exit(1)
			}
		}
	}

	if err := ctxt.Err(); err != nil {
		fmt.Println(err)
		return
	}

	edgecastBaseURL := m3u8Link
	if strings.Contains(edgecastBaseURL, edgecastLinkBaseEndOld) {
		edgecastBaseURL = edgecastBaseURL[0:strings.Index(edgecastBaseURL, edgecastLinkBaseEndOld)]
	} else {
		edgecastBaseURL = edgecastBaseURL[0:strings.Index(edgecastBaseURL, edgecastLinkBaseEnd)]
	}

	if debug {
		fmt.Printf("\nedgecastBaseURL: %s\nm3u8Link: %s\n", edgecastBaseURL, m3u8Link)
	}

	if err := ctxt.Err(); err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println("Getting Video info")

	m3u8List, err := vodStruct.GetM3U8ListForQuality(quality)
	if err != nil {
		fmt.Println("Couldn't download m3u8 list")
		abortWork()
		return
	}

	if debug {
		fmt.Printf("\nm3u8List:\n%s\n", m3u8List)
	}

	var re = regexp.MustCompile("\n([^#]+)\n")
	match := re.FindAllStringSubmatch(m3u8List, -1)

	var m3u8Array []string

	for _, element := range match {
		m3u8Array = append(m3u8Array, element[1])
	}

	if debug {
		fmt.Printf("\nItems list: %v\n", m3u8Array)
	}

	var chunkNum, startChunk int

	if end != "full" {
		targetduration, _ := strconv.Atoi(m3u8List[strings.Index(m3u8List, targetdurationStart)+len(targetdurationStart) : strings.Index(m3u8List, targetdurationEnd)])
		chunkNum = numberOfChunks(vodSH, vodSM, vodSS, vodEH, vodEM, vodES, targetduration)
		startChunk = startingChunk(vodSH, vodSM, vodSS, targetduration)
	} else {
		fmt.Println("Dowbloading full vod")

		chunkNum = len(m3u8Array)
		startChunk = 0
	}

	if debug {
		fmt.Printf("\nchunkNum: %v\nstartChunk: %v\n", chunkNum, startChunk)
	}

	if err := ctxt.Err(); err != nil {
		fmt.Println(err)
		return
	}

	newpath := filepath.Join(".", "_"+vodIDString)

	err = os.MkdirAll(newpath, os.ModePerm)
	if err != nil {
		fmt.Println("Count't create directory")
		os.Exit(1)
	}
	fmt.Printf("Created temp dir: %s\n", newpath)

	cleanUpQueue = append(cleanUpQueue, func() {
		fmt.Println("Deleting temp dir")
		err := os.RemoveAll(newpath)
		if err != nil {
			fmt.Println("Error deleting temp dir in one step.")
			fmt.Println("Deleting chunks")
			deleteChunks(newpath, chunkNum, startChunk, vodIDString)
			fmt.Println("Please delete remaining files manually.")
		}
	})

	fmt.Println("Starting Download")
	totalChunks = chunkNum
	workChan := make(chan func() error, chunkNum)
	for i := startChunk; i < startChunk+chunkNum; i++ {
		s := strconv.Itoa(i)
		n := m3u8Array[i]
		workChan <- func() error {
			return downloadChunk(newpath, edgecastBaseURL, s, n, vodIDString)
		}
	}

	for i := 0; i < maximumConcurrency && i < cap(workChan); i++ {
		wg.Add(1)
		workerID := i
		go func() {
			defer wg.Done()
		loop:
			for {
				select {
				case fn := <-workChan:
					if err := fn(); err != nil {
						fmt.Printf("Worker %d: error: %v\n", workerID, err)
						abortWork()
						break loop
					}
				case <-ctxt.Done():
					if err := ctxt.Err(); err != nil {
						fmt.Printf("Worker %d: abort\n", workerID)
					}
					break loop
				default: // No more work
					break loop
				}
			}
		}()
	}

	wg.Wait()

	if err := ctxt.Err(); err != nil {
		fmt.Println(err)
		return
	}

	defer fmt.Println("All done!")
	fmt.Println("\nCombining parts")
	ffmpegCombine(newpath, chunkNum, startChunk, vodStruct)
	cleanUpAndExit()
}

func rightVersion() bool {
	resp, err := httpClient.Get(currentReleaseLink)
	if err != nil {
		fmt.Println("Couldn't access github while checking for most recent release.")
	}

	body, _ := ioutil.ReadAll(resp.Body)

	respString := string(body)

	cs := strings.Index(respString, currentReleaseStart) + len(currentReleaseStart)
	ce := cs + len(versionNumber)
	return respString[cs:ce] == versionNumber
}

func printQualityOptions(qualityOptions []vod.VodQuality) {
	for _, v := range qualityOptions {
		// fmt.Printf("resolution: %s, download with -quality=\"%s\"\n", v.Resolution, v.Quality)
		fmt.Printf(`%-8s => -quality="%s"`+"\n", v.Resolution, v.Quality)
	}
}

func init() {
	if runtime.GOOS == "windows" {
		ffmpegCMD = `ffmpeg.exe`
	}
}

func main() {

	qualityInfo := flag.Bool("qualityinfo", false, "if you want to see the avaliable quality options")

	standardStartAndEnd := "HH MM SS"
	standardVOD := "123456789"
	vodID := flag.String("vod", standardVOD, "the vod id https://www.twitch.tv/videos/123456789")
	start := flag.String("start", standardStartAndEnd, "For example: 0 0 0 for starting at the bedinning of the vod")
	end := flag.String("end", standardStartAndEnd, "For example: 1 20 0 for ending the vod at 1 hour and 20 minutes")
	quality := flag.String("quality", sourceQuality, "chunked for source quality is automatically used if -quality isn't set")
	flag.BoolVar(&debug, "debug", false, "debug output")
	flag.IntVar(&maximumConcurrency, "concurrency", 5, "Total amount of allowed concurrency for download")
	flag.BoolVar(&useVideoTitle, "videotitle", true, "When set, video will be named like 'This is my VOD_12345678.mp4'")
	flag.BoolVar(&noProgress, "no-progress", false, "When set, video will be named like 'This is my VOD_12345678.mp4'")
	flag.Parse()

	httpClient.Transport = &http.Transport{
		MaxIdleConnsPerHost: maximumConcurrency,
		// MaxIdleConns:        maximumConcurrency,
	}
	vod.SetDebug(debug)
	vod.SetHttpClient(httpClient)

	if !rightVersion() {
		fmt.Printf("\nYou are using an old version of concat. Check out %s for the most recent version.\n\n", currentReleaseLink)
	}

	if *vodID == standardVOD {
		wrongInputNotification()
		os.Exit(1)
	}

	if *qualityInfo {
		vod, err := vod.GetVod(*vodID)
		if err != nil {
			os.Exit(1)
		}
		qualityOptions, err := vod.GetQualityOptions()
		if err != nil {
			os.Exit(1)
		}
		printQualityOptions(qualityOptions)
		os.Exit(0)
	}

	startInterruptWatcher()
	if *start != standardStartAndEnd && *end != standardStartAndEnd {
		downloadPartVOD(*vodID, *start, *end, *quality)
	} else {
		downloadPartVOD(*vodID, "0", "full", *quality)
	}
	// Wait until cleanUpAndExit is called
	<-done
}

func cleanUpAndExit() {
	fmt.Println("Application closing")
	if len(cleanUpQueue) > 0 {
		fmt.Println("Starting cleanup")
		for _, fn := range cleanUpQueue {
			fn()
		}
	}
	close(done)
}

func abortWork() {
	m.Do(func() {
		ctxtAbortFn()
		cleanUpAndExit()
	})
}

func startInterruptWatcher() {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)

	go func(c chan os.Signal) {
		<-c
		fmt.Println("\nReceived abortion signal")
		abortWork()
	}(signalChan)
}
