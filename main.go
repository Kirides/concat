package main

import (
	"bytes"
	"encoding/json"
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
)

//new style of edgecast links: http://vod089-ttvnw.akamaized.net/1059582120fbff1a392a_reinierboortman_26420932624_719978480/chunked/highlight-180380104.m3u8
//old style of edgecast links: http://vod164-ttvnw.akamaized.net/7a16586e4b7ef40300ba_zizaran_27258736688_772341213/chunked/index-dvr.m3u8

const edgecastLinkBegin string = "http://"
const edgecastLinkBaseEndOld string = "index"
const edgecastLinkBaseEnd string = "highlight"
const edgecastLinkM3U8End string = ".m3u8"
const targetdurationStart string = "TARGETDURATION:"
const targetdurationEnd string = "\n#ID3"
const resolutionStart string = `NAME="`
const resolutionEnd string = `"`
const qualityStart string = `VIDEO="`
const qualityEnd string = `"`
const sourceQuality string = "chunked"
const chunkFileExtension string = ".ts"
const currentReleaseLink string = "https://github.com/ArneVogel/concat/releases/latest"
const currentReleaseStart string = `<a href="/ArneVogel/concat/releases/download/`
const currentReleaseEnd string = `/concat"`
const versionNumber string = "v0.2.2"

var ffmpegCMD = `ffmpeg`

var debug bool
var twitchClientID = "aokchnui2n8q38g0vezl9hq6htzy4c"

var cleanUpQueue = make([]func(), 0)
var abort = make(chan struct{})
var done = make(chan struct{}, 1)
var wg sync.WaitGroup

/*
	Returns the signature and token from a tokenAPILink
	signature and token are needed for accessing the usher api
*/
func accessTokenAPI(tokenAPILink string) (string, string, error) {
	if debug {
		fmt.Printf("\ntokenAPILink: %s\n", tokenAPILink)
	}

	resp, err := http.Get(tokenAPILink)
	if err != nil {
		return "", "", err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	// See https://blog.golang.org/json-and-go "Decoding arbitrary data"
	var data interface{}
	err = json.Unmarshal(body, &data)
	m := data.(map[string]interface{})
	sig := fmt.Sprintf("%v", m["sig"])
	token := fmt.Sprintf("%v", m["token"])
	return sig, token, err
}

func accessUsherAPI(usherAPILink string) (map[string]string, error) {
	resp, err := http.Get(usherAPILink)
	if err != nil {
		return make(map[string]string), err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return make(map[string]string), err
	}

	respString := string(body)

	if debug {
		fmt.Printf("\nUsher API response:\n%s\n", respString)
	}

	var re = regexp.MustCompile(qualityStart + "([^\"]+)" + qualityEnd + "\n([^\n]+)\n")
	match := re.FindAllStringSubmatch(respString, -1)

	edgecastURLmap := make(map[string]string)

	for _, element := range match {
		edgecastURLmap[element[1]] = element[2]
	}

	return edgecastURLmap, err
}

func getM3U8List(m3u8Link string) (string, error) {
	resp, err := http.Get(m3u8Link)
	if err != nil {
		return "", err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), err
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

func downloadChunk(newpath string, edgecastBaseURL string, chunkNum string, chunkName string, vodID string) {
	if debug {
		fmt.Printf("Downloading: %s\n", edgecastBaseURL+chunkName)
	} else {
		fmt.Print(".")
	}

	resp, err := http.Get(edgecastBaseURL + chunkName)
	if err != nil {
		os.Exit(1)
	}

	resultFile, err := os.Create(filepath.Join(newpath, vodID+"_"+chunkNum+chunkFileExtension))
	if err != nil {
		fmt.Printf("Could not create file '%s'", newpath+"/"+vodID+"_"+chunkNum+chunkFileExtension)
		os.Exit(1)
	}
	defer resultFile.Close()
	if _, err := io.Copy(resultFile, resp.Body); err != nil {
		fmt.Printf("Could not download file '%s'. %v", vodID+"_"+chunkNum+chunkFileExtension, err)
		close(abort)
	}
}

func createConcatFile(newpath string, chunkNum int, startChunk int, vodID string) (*os.File, error) {
	tempFile, err := ioutil.TempFile(newpath, "twitchVod_"+vodID+"_")
	if err != nil {
		return nil, err
	}
	defer tempFile.Close()
	concatBuf := bytes.NewBuffer(make([]byte, 0))
	for i := startChunk; i < (startChunk + chunkNum); i++ {
		s := strconv.Itoa(i)
		concatBuf.WriteString("file '")
		filePath, _ := filepath.Abs(newpath + "/" + vodID + "_" + s + chunkFileExtension)
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

func ffmpegCombine(newpath string, chunkNum int, startChunk int, vodID string) {
	tempFile, err := createConcatFile(newpath, chunkNum, startChunk, vodID)
	if err != nil {
		fmt.Println(err)
		return
	}
	cleanUpQueue = append(cleanUpQueue, func() {
		os.Remove(tempFile.Name())
	})

	args := []string{"-f", "concat", "-safe", "0", "-i", tempFile.Name(), "-c", "copy", "-bsf:a", "aac_adtstoasc", "-fflags", "+genpts", vodID + ".mp4"}

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

func printQualityOptions(vodIDString string) {
	vodID, _ := strconv.Atoi(vodIDString)

	tokenAPILink := fmt.Sprintf("http://api.twitch.tv/api/vods/%v/access_token?&client_id="+twitchClientID, vodID)

	fmt.Println("Contacting Twitch Server")

	sig, token, err := accessTokenAPI(tokenAPILink)
	if err != nil {
		fmt.Println("Couldn't access twitch token api")
		os.Exit(1)
	}

	usherAPILink := fmt.Sprintf("http://usher.twitch.tv/vod/%v?nauthsig=%v&nauth=%v&allow_source=true", vodID, sig, token)

	resp, err := http.Get(usherAPILink)
	if err != nil {
		return
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}

	respString := string(body)

	qualityCount := strings.Count(respString, resolutionStart)
	for i := 0; i < qualityCount; i++ {
		rs := strings.Index(respString, resolutionStart) + len(resolutionStart)
		re := strings.Index(respString[rs:len(respString)], resolutionEnd) + rs
		qs := strings.Index(respString, qualityStart) + len(qualityStart)
		qe := strings.Index(respString[qs:len(respString)], qualityEnd) + qs

		fmt.Printf("resolution: %s, download with -quality=\"%s\"\n", respString[rs:re], respString[qs:qe])

		respString = respString[qe:len(respString)]
	}
}

func wrongInputNotification() {
	fmt.Println("Call the program with -help for information on how to use it :^)")
}

func downloadPartVOD(vodIDString string, start string, end string, quality string) {
	var vodID, vodSH, vodSM, vodSS, vodEH, vodEM, vodES int

	vodID, _ = strconv.Atoi(vodIDString)

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
			wrongInputNotification()
		}
	}

	_, err := os.Stat(vodIDString + ".mp4")

	if err == nil || !os.IsNotExist(err) {
		fmt.Printf("Destination file %s already exists!\n", vodIDString+".mp4")
		os.Exit(1)
	}

	tokenAPILink := fmt.Sprintf("http://api.twitch.tv/api/vods/%v/access_token?&client_id="+twitchClientID, vodID)

	fmt.Println("Contacting Twitch Server")

	sig, token, err := accessTokenAPI(tokenAPILink)
	if err != nil {
		fmt.Println("Couldn't access twitch token api")
		os.Exit(1)
	}

	if debug {
		fmt.Printf("\nSig: %s, Token: %s\n", sig, token)
	}

	usherAPILink := fmt.Sprintf("http://usher.twitch.tv/vod/%v?nauthsig=%v&nauth=%v&allow_source=true", vodID, sig, token)

	if debug {
		fmt.Printf("\nusherAPILink: %s\n", usherAPILink)
	}

	edgecastURLmap, err := accessUsherAPI(usherAPILink)
	if err != nil {
		fmt.Println("Count't access usher api")
		os.Exit(1)
	}

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
	edgecastBaseURL := m3u8Link
	if strings.Contains(edgecastBaseURL, edgecastLinkBaseEndOld) {
		edgecastBaseURL = edgecastBaseURL[0:strings.Index(edgecastBaseURL, edgecastLinkBaseEndOld)]
	} else {
		edgecastBaseURL = edgecastBaseURL[0:strings.Index(edgecastBaseURL, edgecastLinkBaseEnd)]
	}

	if debug {
		fmt.Printf("\nedgecastBaseURL: %s\nm3u8Link: %s\n", edgecastBaseURL, m3u8Link)
	}

	fmt.Println("Getting Video info")

	m3u8List, err := getM3U8List(m3u8Link)
	if err != nil {
		fmt.Println("Couldn't download m3u8 list")
		os.Exit(1)
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
	workChan := make(chan func(), startChunk+chunkNum)
	for i := startChunk; i < (startChunk + chunkNum); i++ {
		s := strconv.Itoa(i)
		n := m3u8Array[i]
		workChan <- func() {
			downloadChunk(newpath, edgecastBaseURL, s, n, vodIDString)
		}
	}
	downloadStopped := false
	for i := 0; i < 5; i++ {
		wg.Add(1)
		workerID := i
		go func() {
			defer wg.Done()
		loop:
			for {
				select {
				case fn := <-workChan:
					fn()
				case <-abort:
					fmt.Printf("Worker %d: abort\n", workerID)
					downloadStopped = true
					break loop
				default:
					break loop
				}
			}
		}()
	}
	wg.Wait()
	if !downloadStopped {
		fmt.Println("\nCombining parts")
		ffmpegCombine(newpath, chunkNum, startChunk, vodIDString)
		cleanUpAndExit()
		defer fmt.Println("All done!")
	}
}

func rightVersion() bool {
	resp, err := http.Get(currentReleaseLink)
	if err != nil {
		fmt.Println("Couldn't access github while checking for most recent release.")
	}

	body, _ := ioutil.ReadAll(resp.Body)

	respString := string(body)

	cs := strings.Index(respString, currentReleaseStart) + len(currentReleaseStart)
	ce := cs + len(versionNumber)
	return respString[cs:ce] == versionNumber
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
	debugFlag := flag.Bool("debug", false, "debug output")

	flag.Parse()

	debug = *debugFlag

	if !rightVersion() {
		fmt.Printf("\nYou are using an old version of concat. Check out %s for the most recent version.\n\n", currentReleaseLink)
	}

	if *vodID == standardVOD {
		wrongInputNotification()
		os.Exit(1)
	}

	if *qualityInfo {
		printQualityOptions(*vodID)
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
	fmt.Println("Starting cleanup")
	for _, fn := range cleanUpQueue {
		fn()
	}
	close(done)
}

func startInterruptWatcher() {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)

	go func(c chan os.Signal) {
		<-c
		close(abort)
		wg.Wait()
		cleanUpAndExit()
	}(signalChan)
}
