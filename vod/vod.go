package vod

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
)

const resolutionStart = `NAME="`
const resolutionEnd = `"`

const qualityStart = `VIDEO="`
const qualityEnd = `"`
const tokenAPILinkv5 = "https://api.twitch.tv/api/vods/%v/access_token?&client_id=%v"

const usherAPILink = "https://usher.ttvnw.net/vod/%v?nauthsig=%v&nauth=%v&allow_source=true"

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

var (
	persistedQueries = map[string]interface{}{
		"PlaybackAccessToken": map[string]interface{}{
			"version":    1,
			"sha256Hash": "0828119ded1c13477966434e15800ff57ddacf13ba1911c129dc2200705b0712",
		},
	}
)

func makePlaybackAccessTokenQuery(vodID string) []byte {
	q := map[string]interface{}{
		"operationName": "PlaybackAccessToken",
		"variables": map[string]interface{}{
			"isLive":     false,
			"login":      "",
			"isVod":      true,
			"vodID":      vodID,
			"playerType": "channel_home_carousel",
		},
		"extensions": map[string]interface{}{
			"persistedQuery": persistedQueries["PlaybackAccessToken"],
		},
	}

	jsonData, _ := json.Marshal(q)

	return jsonData
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
		vod.Title = data.Title
	}
	return vod, nil
}

type vodDataJson struct {
	Title string `json:"title"`
}

func (vod Vod) fetchData() (*vodDataJson, error) {
	req, err := http.NewRequest("GET", "https://api.twitch.tv/kraken/videos/"+vod.ID, nil)
	if err != nil {
		fmt.Println(err)
	}
	req.Header.Add("Accept", "application/vnd.twitchtv.v5+json")
	req.Header.Add("Client-ID", "aokchnui2n8q38g0vezl9hq6htzy4c")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	buf := bytes.NewBuffer(nil)
	io.Copy(buf, resp.Body)
	var jsonResult vodDataJson
	if err := json.Unmarshal(buf.Bytes(), &jsonResult); err != nil {
		return nil, err
	}

	return &jsonResult, nil
}

// GetQualityOptions Returns all the possible quality options for this VOD
func (vod Vod) GetQualityOptions() ([]Quality, error) {
	fmt.Println("Contacting Twitch Server")

	sig, token, err := vod.accessTokenGQL()
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
		re := strings.Index(respString[rs:], resolutionEnd) + rs
		qs := strings.Index(respString, qualityStart) + len(qualityStart)
		qe := strings.Index(respString[qs:], qualityEnd) + qs

		vodQualities = append(vodQualities, Quality{Resolution: respString[rs:re], Quality: respString[qs:qe]})
		respString = respString[qe:]
	}
	return vodQualities, nil
}

func (vod Vod) newGqlPlaybackAccessTokenRequest() (*http.Request, error) {
	jsonData := makePlaybackAccessTokenQuery(vod.ID)

	req, err := http.NewRequest("POST", "https://gql.twitch.tv/gql", bytes.NewReader(jsonData))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Client-Id", "kimne78kx3ncx6brgo4mv6wki5h1ko")
	return req, nil
}

// AccessTokenAPIv5 Returns the signature and token from a tokenAPILink signature and token are needed for accessing the usher api
func (vod Vod) accessTokenGQL() (string, string, error) {
	req, err := vod.newGqlPlaybackAccessTokenRequest()
	if err != nil {
		return "", "", err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	type gqlVideoPlaybackAccessToken struct {
		Value     string `json:"value"`
		Signature string `json:"signature"`
	}
	type gqlResponseData struct {
		VideoPlaybackAccessToken gqlVideoPlaybackAccessToken `json:"videoPlaybackAccessToken"`
	}
	type gqlResponse struct {
		Data gqlResponseData `json:"data"`
	}
	var data gqlResponse
	err = json.Unmarshal(body, &data)
	if err != nil {
		return "", "", err
	}
	if data.Data.VideoPlaybackAccessToken.Signature == "" || data.Data.VideoPlaybackAccessToken.Value == "" {
		return "", "", fmt.Errorf("Could not get acces token data")
	}
	return data.Data.VideoPlaybackAccessToken.Signature, data.Data.VideoPlaybackAccessToken.Value, err
}

func SetDebug(v bool) {
	debug = v
}

// GetEdgecastURLMap ...
func (vod Vod) GetEdgecastURLMap() map[string]string {
	return vod.apiMap
}

func (vod Vod) getEdgecastURLMap() (map[string]string, error) {
	sig, token, err := vod.accessTokenGQL()
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

	var re = regexp.MustCompile(`VIDEO="([^"]+)".*\n([^\n]+)`)
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
