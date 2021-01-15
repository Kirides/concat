package vod

import (
	"testing"
)

// use channel twitch.tv/reckful for testing because he has a legacy account where vods aren't deleted
const vodString string = "187938112"
const vodInt int = 187938112

func TestTokenAPILink(t *testing.T) {
	testVod, _ := GetVod(vodString)
	exampleSig := "7ce3d0ca2c65dd66c7da72c43f4ce72cfcd98a72"
	sig, _, err := testVod.accessTokenGQL()
	// Testing for length of sig because the sig from twitch is random
	// Havent come up with a meaningful test for token
	if err != nil || len(sig) != len(exampleSig) {
		t.Errorf("Error in accessTokenAPI")
	}
}
