package vod

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"reflect"
	"regexp"
	"strings"
)

const resolutionStart = `NAME="`
const resolutionEnd = `"`

const qualityStart = `VIDEO="`
const qualityEnd = `"`
const tokenAPILinkv5 = "https://api.twitch.tv/api/vods/%v/access_token?&client_id=%v"

// const usherAPILink = "https://usher.twitch.tv/vod/%v?nauthsig=%v&nauth=%v&allow_source=true"
const usherAPILink = "https://usher.ttvnw.net/vod/%v?nauthsig=%v&nauth=%v&allow_source=true"

// TwitchClientID defines the ID used for interacting with the Twitch-API
var TwitchClientID = ""

// TwitchClientSecret defines the Client Secret used for interacting with the Twitch-API
var TwitchClientSecret = ""
var debug = false
var httpClient = http.DefaultClient

// Vod is a struct that enables object-oriented access to the VOD
type Vod struct {
	ID     string
	apiMap map[string]string
	Title  string
}

// Quality a struct that contains quality values of a VOD
type Quality struct {
	Resolution string
	Quality    string
}

// GetM3U8ListForQuality ...
func (vod Vod) GetM3U8ListForQuality(quality string) (string, error) {
	resp, err := httpClient.Get(vod.apiMap[quality])
	if err != nil {
		return "", err
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), err
}

// GetVod returns a Vod-Struct that
func GetVod(id string) (Vod, error) {
	vod := Vod{ID: id}
	apiMap, err := vod.getEdgecastURLMap()
	if err != nil {
		return vod, err
	}
	vod.apiMap = apiMap

	if data, err := vod.fetchData(); err == nil {
		title, ok := data["title"]
		if ok && reflect.TypeOf(title).Kind() == reflect.String {
			vod.Title = title.(string)
		}
	}
	return vod, nil
}

func (vod Vod) fetchData() (map[string]interface{}, error) {
	req, err := http.NewRequest("GET", "https://api.twitch.tv/kraken/videos/"+vod.ID, nil)
	if err != nil {
		fmt.Println(err)
	}
	req.Header.Add("Accept", "application/vnd.twitchtv.v5+json")
	req.Header.Add("Client-ID", TwitchClientID)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	buf := bytes.NewBuffer(nil)
	io.Copy(buf, resp.Body)
	var jsonResult interface{}
	if err := json.Unmarshal(buf.Bytes(), &jsonResult); err != nil {
		return nil, err
	}

	return jsonResult.(map[string]interface{}), nil
}

// GetQualityOptions Returns all the possible quality options for this VOD
func (vod Vod) GetQualityOptions() ([]Quality, error) {
	fmt.Println("Contacting Twitch Server")

	sig, token, err := vod.AccessTokenAPIv5()
	if err != nil {
		fmt.Println("Couldn't access twitch token api")
		return nil, err
	}

	respString, err := vod.accessUsherAPIRaw(sig, token)
	if err != nil {
		return nil, err
	}

	qualityCount := strings.Count(respString, resolutionStart)
	vodQualities := make([]Quality, 0)
	for i := 0; i < qualityCount; i++ {
		rs := strings.Index(respString, resolutionStart) + len(resolutionStart)
		re := strings.Index(respString[rs:len(respString)], resolutionEnd) + rs
		qs := strings.Index(respString, qualityStart) + len(qualityStart)
		qe := strings.Index(respString[qs:len(respString)], qualityEnd) + qs

		vodQualities = append(vodQualities, Quality{Resolution: respString[rs:re], Quality: respString[qs:qe]})
		respString = respString[qe:len(respString)]
	}
	return vodQualities, nil
}

// AccessTokenAPIv5 Returns the signature and token from a tokenAPILink signature and token are needed for accessing the usher api
func (vod Vod) AccessTokenAPIv5() (string, string, error) {
	resp, err := httpClient.Get(fmt.Sprintf(tokenAPILinkv5, vod.ID, TwitchClientID))
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

// GetEdgecastURLMap ...
func (vod Vod) GetEdgecastURLMap() map[string]string {
	return vod.apiMap
}

func (vod Vod) getEdgecastURLMap() (map[string]string, error) {
	sig, token, err := vod.AccessTokenAPIv5()
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
	resp, err := httpClient.Get(fmt.Sprintf(usherAPILink, vod.ID, signature, token))
	if err != nil {
		return "", err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// SetHTTPClient sets the used http.Client for api requests
func SetHTTPClient(client *http.Client) {
	httpClient = client
}
