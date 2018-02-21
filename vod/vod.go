package vod

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
)

const resolutionStart = `NAME="`
const resolutionEnd = `"`

const qualityStart = `VIDEO="`
const qualityEnd = `"`
const tokenAPILink = "http://api.twitch.tv/api/vods/%v/access_token?&client_id=%v"
const usherAPILink = "http://usher.twitch.tv/vod/%v?nauthsig=%v&nauth=%v&allow_source=true"

var TwitchClientID = "aokchnui2n8q38g0vezl9hq6htzy4c"
var debug = false

type Vod struct {
	ID     string
	apiMap map[string]string
}
type VodQuality struct {
	Resolution string
	Quality    string
}

func (vod Vod) GetM3U8ListForQuality(quality string) (string, error) {
	resp, err := http.Get(vod.apiMap[quality])
	if err != nil {
		return "", err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), err
}

func GetVod(id string) (Vod, error) {
	vod := Vod{ID: id}
	apiMap, err := vod.getEdgecastURLMap()
	if err != nil {
		return vod, err
	}
	vod.apiMap = apiMap

	return vod, nil
}

func (vod Vod) GetQualityOptions() ([]VodQuality, error) {
	fmt.Println("Contacting Twitch Server")

	sig, token, err := vod.AccessTokenAPI()
	if err != nil {
		fmt.Println("Couldn't access twitch token api")
		return nil, err
	}

	respString, err := vod.accessUsherAPIRaw(sig, token)
	if err != nil {
		return nil, err
	}

	qualityCount := strings.Count(respString, resolutionStart)
	vodQualities := make([]VodQuality, 0)
	for i := 0; i < qualityCount; i++ {
		rs := strings.Index(respString, resolutionStart) + len(resolutionStart)
		re := strings.Index(respString[rs:len(respString)], resolutionEnd) + rs
		qs := strings.Index(respString, qualityStart) + len(qualityStart)
		qe := strings.Index(respString[qs:len(respString)], qualityEnd) + qs

		vodQualities = append(vodQualities, VodQuality{Resolution: respString[rs:re], Quality: respString[qs:qe]})
		respString = respString[qe:len(respString)]
	}
	return vodQualities, nil
}

/*
	Returns the signature and token from a tokenAPILink
	signature and token are needed for accessing the usher api
*/
func (vod Vod) AccessTokenAPI() (string, string, error) {
	resp, err := http.Get(fmt.Sprintf(tokenAPILink, vod.ID, TwitchClientID))
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

func SetDebug(v bool) {
	debug = v
}

func (vod Vod) GetEdgecastURLMap() map[string]string {
	return vod.apiMap
}

func (vod Vod) getEdgecastURLMap() (map[string]string, error) {
	sig, token, err := vod.AccessTokenAPI()
	if err != nil {
		return make(map[string]string), err
	}
	respString, err := vod.accessUsherAPIRaw(sig, token)
	if err != nil {
		return make(map[string]string), err
	}
	if debug {
		fmt.Printf("\nUsher API response:\n%s\n", respString)
	}

	var re = regexp.MustCompile(qualityStart + "([^\"]+)" + qualityEnd + "\n([^\n]+)\n")
	match := re.FindAllStringSubmatch(respString, -1)

	edgecastURLmap := make(map[string]string)

	for _, element := range match {
		edgecastURLmap[element[1]] = element[2]
	}
	return edgecastURLmap, nil
}

func (vod Vod) accessUsherAPIRaw(signature, token string) (string, error) {
	resp, err := http.Get(fmt.Sprintf(usherAPILink, vod.ID, signature, token))
	if err != nil {
		return "", err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
