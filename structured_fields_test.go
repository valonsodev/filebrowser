package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUploadStructuredFields(t *testing.T) {
	for _, tc := range []struct {
		input string
		value int64
		valid bool
	}{
		{"0", 0, true}, {"42", 42, true}, {"999999999999999", 999999999999999, true},
		{"42;extension=token", 42, true}, {"1000000000000000", 0, false},
		{"-1", 0, false}, {"+1", 0, false}, {"1.0", 0, false},
		{"\"42\"", 0, false}, {"1,2", 0, false}, {"", 0, false},
	} {
		t.Run("integer/"+tc.input, func(t *testing.T) {
			value, valid := parseSFInteger(tc.input)
			if valid != tc.valid || (valid && value != tc.value) {
				t.Fatalf("parse(%q) = %d, %t", tc.input, value, valid)
			}
		})
	}
	for _, tc := range []struct {
		input        string
		value, valid bool
	}{
		{"?1", true, true}, {"?0", false, true}, {"?1;extension=token", true, true},
		{"true", false, false}, {"1", false, false}, {"?2", false, false},
		{"?1,?0", false, false}, {"", false, false},
	} {
		t.Run("boolean/"+tc.input, func(t *testing.T) {
			value, valid := parseSFBool(tc.input)
			if valid != tc.valid || (valid && value != tc.value) {
				t.Fatalf("parse(%q) = %t, %t", tc.input, value, valid)
			}
		})
	}
}

func TestUploadRejectsRepeatedStructuredFields(t *testing.T) {
	securityWorkspace(t)
	for _, header := range []string{"Upload-Complete", "Upload-Length"} {
		r := httptest.NewRequest(http.MethodPost, "/test.bin", strings.NewReader(""))
		r.Header.Set("Upload-Complete", "?0")
		r.Header.Set("Upload-Length", "3")
		r.Header.Add(header, r.Header.Get(header))
		w := httptest.NewRecorder()
		pathHandler(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("repeated %s: status = %d", header, w.Code)
		}
	}
	location := hashCreation("/test.bin", "abc", false).Header().Get("Location")
	r := httptest.NewRequest(http.MethodPatch, location, strings.NewReader("abc"))
	r.Header.Set("Content-Type", partialUploadMediaType)
	r.Header.Set("Upload-Complete", "?1")
	r.Header.Add("Upload-Offset", "0")
	r.Header.Add("Upload-Offset", "0")
	w := httptest.NewRecorder()
	pathHandler(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("repeated offset: status = %d", w.Code)
	}
	u, ok := registry.get(strings.TrimPrefix(location, uploadURLPrefix))
	if !ok || u.offset != 0 {
		t.Fatal("invalid header advanced the upload")
	}
}
