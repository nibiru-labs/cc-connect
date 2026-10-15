package core

import "testing"

func TestHasPlatformForSession(t *testing.T) {
	e := NewEngine("t", &controllableAgent{}, []Platform{&stubPlatformEngine{n: "dingtalk"}}, "", LangEnglish)
	for key, want := range map[string]bool{
		"dingtalk:d:cid:123":       true,
		"ws1:dingtalk:d:cid:123":   true,
		"weixin:dm:o9cq@im.wechat": false,
		"":                         false,
	} {
		if got := e.HasPlatformForSession(key); got != want {
			t.Errorf("%q: got %v want %v", key, got, want)
		}
	}
}
