package main

import (
	"strings"
	"testing"

	"github.com/ArneVogel/concat/vod"
)

// use channel twitch.tv/reckful for testing because he has a legacy account where vods aren't deleted
const vodString string = "187938112"
const vodInt int = 187938112

func TestAccessUsherAPI(t *testing.T) {
	testVod, _ := vod.GetVod(vodString)

	edgecastURLmap := testVod.GetEdgecastURLMap()

	m3u8Link, _ := edgecastURLmap["chunked"]

	edgecastBaseURL := m3u8Link
	edgecastBaseURL = edgecastBaseURL[0:strings.Index(edgecastBaseURL, edgecastLinkBaseEnd)]

	//Only checking the end because both
	//http://fastly.vod.hls.ttvnw.net/903cba256ea3055674be_reckful_26660278144_734937575/chunked/
	//http://vod142-ttvnw.akamaized.net/903cba256ea3055674be_reckful_26660278144_734937575/chunked/
	//are valid results
	baseURLEnd := "903cba256ea3055674be_reckful_26660278144_734937575/chunked/"
	//Same with m3u8 Link
	m3u8LinkEnd := "/903cba256ea3055674be_reckful_26660278144_734937575/chunked/index-dvr.m3u8"
	if edgecastBaseURL[len(edgecastBaseURL)-len(baseURLEnd):] != baseURLEnd || m3u8Link[len(m3u8Link)-len(m3u8LinkEnd):] != m3u8LinkEnd {
		t.Errorf("Error in AccessUsherAPI, got baseUrl: %s, m3u8Link: %s", edgecastBaseURL, m3u8Link)
	}
}
