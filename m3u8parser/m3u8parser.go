package m3u8parser

import (
	"fmt"
	"regexp"
	"strconv"
)

// Parse parses the m3u8 file and returns urls and their durations
func Parse(m3u8 string) ([]string, []float64, error) {
	urls := readFileUris(m3u8)
	durations, err := readFileDurations(m3u8)
	return urls, durations, err
}

func readFileUris(m3u8List string) []string {
	var fileRegex = regexp.MustCompile("(?m:^[^#\\n]+)")
	matches := fileRegex.FindAllStringSubmatch(m3u8List, -1)
	var ret []string
	for _, match := range matches {
		ret = append(ret, match[0])
	}
	return ret
}

func readFileDurations(m3u8List string) ([]float64, error) {
	var fileRegex = regexp.MustCompile("(?m:^#EXTINF:(\\d+(\\.\\d+)?))")
	matches := fileRegex.FindAllStringSubmatch(m3u8List, -1)

	var ret []float64

	for _, match := range matches {

		fileLength, err := strconv.ParseFloat(match[1], 64)

		if err != nil {
			return nil, fmt.Errorf("Could not parse %s to a float. %v", match[1], err)
		}

		ret = append(ret, fileLength)
	}

	return ret, nil
}
