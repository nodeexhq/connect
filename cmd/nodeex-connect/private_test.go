package main

import (
	"encoding/json"
	"testing"
)

func TestValidDisk(t *testing.T) {
	for s, want := range map[string]bool{
		"disk3": true, "disk3s7": true, "disk12s1": true,
		"disk": false, "disk3;rm": false, "disk3 s1": false, "/dev/disk3": false, "": false, "disk3$(x)": false,
	} {
		if got := validDisk(s); got != want {
			t.Errorf("validDisk(%q) = %v", s, got)
		}
	}
}

// The shape diskutil apfs list -plist has once plutil turns it into JSON.
func TestAPFSListParse(t *testing.T) {
	raw := `{"Containers":[{"ContainerReference":"disk3","Volumes":[
		{"Name":"Macintosh HD","DeviceIdentifier":"disk3s1","Locked":false},
		{"Name":"NodeEx Private","DeviceIdentifier":"disk3s7","Locked":true,"Encryption":true}]}]}`
	var l apfsList
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		t.Fatal(err)
	}
	v := l.Containers[0].Volumes[1]
	if v.Name != privateVolume || v.DeviceIdentifier != "disk3s7" || !v.Locked {
		t.Errorf("parsed %+v", v)
	}
}
