package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ArneVogel/concat/m3u8parser"
	progressbar "github.com/schollz/progressbar/v3"

	"github.com/ArneVogel/concat/vod"
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
const maxRetryCount int = 3

var ffmpegCMD = `ffmpeg`

var debug bool
var maximumConcurrency int
var useVideoTitle bool
var noProgress bool
var pb *progressbar.ProgressBar

var m sync.Once
var ctxt, ctxtAbortFn = context.WithCancel(context.Background())
var closed = false
var cleanUpQueue = make([]func(), 0)
var done = make(chan struct{})
var wg sync.WaitGroup
var httpClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 2500 * time.Millisecond, // TCP connect timeout
		}).DialContext,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
	},
}
var totalChunks int
var chunksCompleted int32 = 0

var vodTimeFormat = "HH MM SS"

func resetVars() {
	totalChunks = 0
	chunksCompleted = 0
}

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

func chunkFmt(chunk int) string {
	return fmt.Sprintf("%09d", chunk)
}

type inMemoryBuffer struct {
	b *bytes.Buffer
}

func newInMemoryBuffer() *inMemoryBuffer {
	return &inMemoryBuffer{
		b: new(bytes.Buffer),
	}
}
func (b *inMemoryBuffer) Reset() {
	b.b.Reset()
}
func (b *inMemoryBuffer) Write(buf []byte) (int, error) {
	n, err := b.b.Write(buf)
	return n, err
}
func (b *inMemoryBuffer) Size() int64 {
	return int64(len(b.Bytes()))
}
func (b *inMemoryBuffer) Bytes() []byte {
	return b.b.Bytes()
}
func downloadChunk(edgecastBaseURL string, chunkNum string, chunkName string, vodID string, buf *inMemoryBuffer) error {
	if debug {
		fmt.Printf("Downloading: %s\n", edgecastBaseURL+chunkName)
	}
	retry := 0
	for {
		req, err := http.NewRequest("GET", edgecastBaseURL+chunkName, nil)
		if err != nil {
			if strings.Contains(err.Error(), "canceled") {
				return nil
			}
			return fmt.Errorf("Could not reach %s: '%v'", edgecastBaseURL+chunkName, err)
		}

		if buf.Size() != 0 {
			rangeHeader := "bytes=" + strconv.FormatInt(buf.Size(), 10) + "-"
			req.Header.Add("range", rangeHeader)
		}

		req = req.WithContext(ctxt)
		resp, err := httpClient.Do(req)
		if err != nil {
			if strings.Contains(err.Error(), "canceled") {
				return nil
			}
			return fmt.Errorf("Could not get '%v'", err)
		}
		closer := func() {
			resp.Body.Close()
		}
		defer closer() // Ensure file will be closed
		if _, err := io.Copy(buf, resp.Body); err != nil {
			retry++
			isCancelled := strings.Contains(err.Error(), "canceled")
			if retry > maxRetryCount || isCancelled {
				closer()
				if isCancelled {
					return nil
				}
				return fmt.Errorf("Could not download chunk '%s'. %v", chunkNum, err)
			}
			fmt.Printf("Error downloading (retry: %d) file: %s\n", retry, edgecastBaseURL+chunkName)
			continue
		}
		closer() // close file prior to moving
		break
	}
	atomic.AddInt32(&chunksCompleted, 1)
	if !debug && !noProgress {
		pb.Add(1)
	}
	return nil
}

func sanitizeFilename(fileName string) string {
	invalidLetters := [...]string{`?`, `\`, `/`, `:`, `*`, `>`, `<`, `|`, "\x00"}
	sanitized := fileName
	for _, l := range invalidLetters {
		sanitized = strings.Replace(sanitized, l, "", -1)
	}
	return sanitized
}

func wrongInputNotification() {
	fmt.Println("Call the program with -help for information on how to use it :^)")
}

func downloadPartVOD(vodIDString string, start string, end string, quality string) {
	var vodSH, vodSM, vodSS, vodEH, vodEM, vodES int
	if start != vodTimeFormat {
		startArray := strings.Split(start, " ")
		vodSH, _ = strconv.Atoi(startArray[0]) //start Hour
		vodSM, _ = strconv.Atoi(startArray[1]) //start minute
		vodSS, _ = strconv.Atoi(startArray[2]) //start second
	}

	if end != "full" {
		endArray := strings.Split(end, " ")
		vodEH, _ = strconv.Atoi(endArray[0]) //end hour
		vodEM, _ = strconv.Atoi(endArray[1]) //end minute
		vodES, _ = strconv.Atoi(endArray[2]) //end second

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
	fileName := vodIDString + ".ts"
	if useVideoTitle {
		formatString := `%s_%s.ts`
		if vodStruct.Title == "" { // If for some reason, the title could not be fetched
			formatString = `%s%s.ts`
		}
		fileName = fmt.Sprintf(formatString, sanitizeFilename(vodStruct.Title), vodStruct.ID)
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
	vodTitle := vodStruct.ID
	if useVideoTitle {
		vodTitle = vodStruct.Title
	}
	fmt.Printf("Getting Video info for '%s'\n", vodTitle)

	m3u8List, err := vodStruct.GetM3U8ListForQuality(quality)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Couldn't download m3u8 list")
		abortWork()
		return
	}

	if debug {
		fmt.Printf("\nm3u8List:\n%s\n", m3u8List)
	}

	m3u8Array, fileDurations, err := m3u8parser.Parse(m3u8List)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not parse durations")
		abortWork()
		return
	}
	if debug {
		fmt.Printf("\nItems list: %v\n", m3u8Array)
	}

	var chunkNum, startChunk int

	if end != "full" {
		// targetduration, _ := strconv.Atoi(m3u8List[strings.Index(m3u8List, targetdurationStart)+len(targetdurationStart) : strings.Index(m3u8List, targetdurationEnd)])
		// fmt.Printf("TargetDuration: %d\n", targetduration)
		// chunkNum = numberOfChunks(vodSH, vodSM, vodSS, vodEH, vodEM, vodES, targetduration)
		// startChunk = startingChunk(vodSH, vodSM, vodSS, targetduration)

		if err != nil || len(fileDurations) != len(m3u8Array) {
			dbgPrintf("Could not determine real file durations. Using targetDuration as fallback.")
			targetduration, _ := strconv.Atoi(m3u8List[strings.Index(m3u8List, targetdurationStart)+len(targetdurationStart) : strings.Index(m3u8List, targetdurationEnd)])
			chunkNum = calcChunkCount(vodSH, vodSM, vodSS, vodEH, vodEM, vodES, targetduration)
			startChunk = startingChunk(vodSH, vodSM, vodSS, targetduration)
		} else {
			startSeconds := toSeconds(vodSH, vodSM, vodSS)
			clipDuration := toSeconds(vodEH, vodEM, vodES) - startSeconds
			startChunk, chunkNum, _ = calcStartChunkAndChunkCount(fileDurations, startSeconds, clipDuration)
		}
	} else {
		if start != vodTimeFormat {
			fmt.Printf("Downloading from %02d:%02d:%02d until end\n", vodSH, vodSM, vodSS)
			if err != nil || len(fileDurations) != len(m3u8Array) {
				dbgPrintf("Could not determine real file durations. Using targetDuration as fallback.")
				targetduration, _ := strconv.Atoi(m3u8List[strings.Index(m3u8List, targetdurationStart)+len(targetdurationStart) : strings.Index(m3u8List, targetdurationEnd)])
				startChunk = startingChunk(vodSH, vodSM, vodSS, targetduration)
				chunkNum = len(m3u8Array[startChunk:])
			} else {
				startSeconds := toSeconds(vodSH, vodSM, vodSS)
				startChunk, _, _ = calcStartChunkAndChunkCount(fileDurations, startSeconds, 0)
				chunkNum = len(m3u8Array[startChunk:])
			}

		} else {
			fmt.Println("Downloading full vod")
			chunkNum = len(m3u8Array)
			startChunk = 0
		}
	}

	if debug {
		fmt.Printf("\nchunkNum: %v\nstartChunk: %v\n", chunkNum, startChunk)
	}

	if err := ctxt.Err(); err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println("Starting Download")
	totalChunks = chunkNum
	workChan := make(chan func(*inMemoryBuffer) error, chunkNum)
	nextChunk := int32(startChunk)

	pb.ChangeMax(totalChunks)
	outFile, err := os.Create(fileName)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer outFile.Close()
	for i := startChunk; i < startChunk+chunkNum; i++ {
		n := m3u8Array[i]
		cur := i
		workChan <- func(buf *inMemoryBuffer) error {
			buf.Reset()
			err := downloadChunk(edgecastBaseURL, chunkFmt(cur), n, vodIDString, buf)
			if err != nil {
				return err
			}
			ticker := time.NewTicker(120 * time.Second)
			defer ticker.Stop()

			for int32(cur) != nextChunk {
				select {
				case <-ticker.C:
					return fmt.Errorf("Waiting for consecutive parts took too long")
				default:
					time.Sleep(50 * time.Millisecond)
				}
			}
			n, err := io.Copy(outFile, bytes.NewReader(buf.Bytes()))
			if err != nil {
				return err
			}
			if n != buf.Size() {
				return fmt.Errorf("Not all bytes copied to outFile")
			}
			atomic.AddInt32(&nextChunk, 1)
			return nil
		}
	}
	for i := 0; i < maximumConcurrency && i < cap(workChan); i++ {
		wg.Add(1)
		workerID := i
		buf := newInMemoryBuffer()
		go func(workQueue <-chan func(*inMemoryBuffer) error, done <-chan struct{}) {
			defer wg.Done()
			for {
				select {
				case fn := <-workQueue:
					if err := fn(buf); err != nil {
						fmt.Printf("Worker %d: error: %v\n", workerID, err)
						abortWork()
						return
					}
				case <-done:
					if err := ctxt.Err(); err != nil {
						fmt.Printf("Worker %d: abort\n", workerID)
					}
					return
				default:
					return
				}
			}
		}(workChan, ctxt.Done())
	}

	wg.Wait()

	if err := ctxt.Err(); err != nil {
		fmt.Println(err)
		return
	}
}

func dbgPrintf(format string, a ...interface{}) (int, error) {
	if debug {
		return fmt.Printf(format, a...)
	}
	return 0, nil
}

func calcStartChunkAndChunkCount(chunkDurations []float64, startSeconds int, clipDuration int) (int, int, float64) {
	startChunk := 0
	chunkCount := 0
	startSecondsRemainder := float64(0)

	cumulatedDuration := 0.0
	for chunk, chunkDuration := range chunkDurations {
		cumulatedDuration += chunkDuration

		if cumulatedDuration > float64(startSeconds) {
			startChunk = chunk
			startSecondsRemainder = float64(startSeconds) - (cumulatedDuration - chunkDuration)
			break
		}
	}

	cumulatedDuration = 0.0
	minChunkedClipDuration := float64(clipDuration) + startSecondsRemainder
	for chunk := startChunk; chunk < len(chunkDurations); chunk++ {
		cumulatedDuration += chunkDurations[chunk]

		if cumulatedDuration > minChunkedClipDuration {
			chunkCount = chunk - startChunk + 1
			break
		}
	}

	if chunkCount == 0 {
		chunkCount = len(chunkDurations) - startChunk
	}

	return startChunk, chunkCount, startSecondsRemainder
}

func calcChunkCount(sh int, sm int, ss int, eh int, em int, es int, target int) int {
	startSeconds := toSeconds(sh, sm, ss)
	endSeconds := toSeconds(eh, em, es)

	return ((endSeconds - startSeconds) / target) + 1
}

func toSeconds(sh int, sm int, ss int) int {
	return sh*3600 + sm*60 + ss
}

func defaultEnd(v string) bool {
	return (v == vodTimeFormat)
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

func printQualityOptions(qualityOptions []vod.Quality) {
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
	var qualityInfo bool
	var start, end, quality string
	standardVOD := ""

	flag.Usage = printUsage

	flag.BoolVar(&qualityInfo, "qualityinfo", false, "if you want to see the avaliable quality options")
	flag.StringVar(&start, "start", "0 0 0", "\"HH mm ss\" For example: 0 0 0 for starting at the bedinning of the vod")
	flag.StringVar(&end, "end", "full", "For example: 1 20 0 for ending the vod at 1 hour and 20 minutes")
	flag.StringVar(&quality, "quality", sourceQuality, "chunked for source quality is automatically used if -quality isn't set")
	flag.BoolVar(&debug, "debug", false, "debug output")
	flag.IntVar(&maximumConcurrency, "concurrency", 5, "Total amount of allowed concurrency for download")
	flag.BoolVar(&useVideoTitle, "videotitle", true, "When set, video will be named like 'This is my VOD_12345678.mp4'")
	flag.BoolVar(&noProgress, "no-progress", false, "Do not display progressbar while download")
	flag.Parse()

	httpClient.Transport = &http.Transport{
		MaxIdleConnsPerHost: maximumConcurrency,
		// MaxIdleConns:        maximumConcurrency,
	}
	vod.SetDebug(debug)
	vod.SetHTTPClient(httpClient)

	// if !rightVersion() {
	// 	fmt.Printf("\nYou are not using the latest version of concat. Check out %s for the most recent version.\n\n", currentReleaseLink)
	// }

	handleInterrupt(func() {
		fmt.Println("\nReceived abortion signal")
		abortWork()
	})
	pb = progressbar.NewOptions(0,
		progressbar.OptionClearOnFinish(),
		progressbar.OptionFullWidth(),
		progressbar.OptionShowCount(),
		progressbar.OptionShowIts(),
		progressbar.OptionThrottle(200*time.Millisecond),
		progressbar.OptionUseANSICodes(true),
		progressbar.OptionSetItsString("chunks"),
	)

	args := flag.Args()
	// args = []string{"898132500"}
	for _, vodID := range args {
		pb.Reset()
		resetVars()
		if vodID == standardVOD {
			wrongInputNotification()
			os.Exit(1)
		}
		vodID, _ = normalizeVodID(vodID)
		if qualityInfo {
			vod, err := vod.GetVod(vodID)
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
		pb.Describe(vodID)
		if start != vodTimeFormat && end != vodTimeFormat {
			downloadPartVOD(vodID, start, end, quality)
		} else {
			downloadPartVOD(vodID, "0", "full", quality)
		}
		pb.Finish()

		cleanUp()
	}
	abortWork()

	// Wait until abortWork is called
	<-done
}

func printUsage() {
	fmt.Printf("Usage: %s [OPTIONS] urlOrID ... nUrlOrID\n", os.Args[0])
	flag.PrintDefaults()
}

func normalizeVodID(vodID string) (string, error) {
	rxURL := regexp.MustCompile(`twitch.tv\/videos\/([\w\-_]+)`)
	rxID := regexp.MustCompile(`[\w\-_]+`)

	if strings.HasPrefix(vodID, "http") && rxURL.MatchString(vodID) {
		if match := rxURL.FindStringSubmatch(vodID); match != nil {
			return match[1], nil
		}
	}
	if rxID.MatchString(vodID) {
		return vodID, nil
	}

	return vodID, fmt.Errorf("Invalid Video ID/URL '%s'", vodID)
}

func cleanUpAndExit() {
	fmt.Println("Application closing")
	cleanUp()
	close(done)
}

func cleanUp() {
	if cleanUpQueue != nil && len(cleanUpQueue) > 0 {
		fmt.Println("Starting cleanup")
		for _, fn := range cleanUpQueue {
			fn()
		}
		cleanUpQueue = nil
	}
}

func abortWork() {
	m.Do(func() {
		ctxtAbortFn()
		cleanUpAndExit()
	})
}

func handleInterrupt(onInterrupt func()) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGKILL)

	go func() {
		defer cancel()
		<-ctx.Done()
		onInterrupt()
	}()
}
