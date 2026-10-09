package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUploadRoutesRejectOversizedRequestBeforeParsing(t *testing.T) {
	for _,path:=range []string{"/api/instances","/api/instances/1/power","/api/instances/1/import"} {
		t.Run(path,func(t *testing.T){
			s:=agentForTest(t,&testDriver{})
			req:=httptest.NewRequest(http.MethodPost,path,strings.NewReader("do not parse"))
			req.ContentLength=maxImageSize+(1<<20)+1
			req.Header.Set("Content-Type","multipart/form-data; boundary=test")
			req.Header.Set("X-Agent-Token","token")
			r:=httptest.NewRecorder()
			s.handler().ServeHTTP(r,req)
			if r.Code!=http.StatusRequestEntityTooLarge {t.Fatalf("oversized request = %d, want 413: %s",r.Code,r.Body.String())}
		})
	}
}
