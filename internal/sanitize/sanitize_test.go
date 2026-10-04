package sanitize

import (
	"InfraPulseAgent/internal/model"
	"strings"
	"testing"
)

func TestKnownSecrets(t *testing.T) {
	s := Text(`password=hunter2 token="private value" community: public Authorization: Bearer abc.def {"password":"json-value","token":"json-token"} Bearer abc~def`)
	for _, v := range []string{"hunter2", "private value", "public", "abc.def", "json-value", "json-token", "abc~def"} {
		if strings.Contains(s, v) {
			t.Fatalf("secret leaked: %s", v)
		}
	}
}
func TestNoCallerMutation(t *testing.T) {
	e := model.Envelope{Events: []model.Event{{Message: "token=secret"}}}
	out := (KnownSecrets{}).Sanitize(e)
	if out.Events[0].Message == e.Events[0].Message || e.Events[0].Message != "token=secret" {
		t.Fatal("mutation/redaction")
	}
}
