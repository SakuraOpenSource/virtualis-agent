package main

import (
	"context"
	"flag"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type startupRoundTripper func(*http.Request)(*http.Response,error)
func (f startupRoundTripper) RoundTrip(r *http.Request)(*http.Response,error){return f(r)}

func TestRegisterRejectsNonLoopbackHTTPBeforeSendingToken(t *testing.T) {
	transport:=http.DefaultTransport
	defer func(){http.DefaultTransport=transport}()
	sent:=false
	http.DefaultTransport=startupRoundTripper(func(*http.Request)(*http.Response,error){sent=true;return &http.Response{StatusCode:200,Body:io.NopCloser(strings.NewReader("{}"))},nil})
	s:=agentForTest(t,&testDriver{})
	err:=s.register(context.Background(),"http://node.invalid:8080","http://127.0.0.1:8081")
	if err==nil || !strings.Contains(err.Error(),"--allow-insecure") || sent {t.Fatalf("plaintext master not rejected before token transmission: err=%v sent=%v",err,sent)}
}

func TestAgentStartupHelper(t *testing.T) {
	if os.Getenv("VIRTUALIS_STARTUP_HELPER")!="1" {return}
	flag.CommandLine=flag.NewFlagSet("agent",flag.ExitOnError)
	os.Args=[]string{"agent","--master=http://node.invalid:8080","--token-file="+os.Getenv("VIRTUALIS_STARTUP_TOKEN_FILE"),"--listen=127.0.0.1:0","--data="+os.Getenv("VIRTUALIS_STARTUP_DATA")}
	main()
	os.Exit(0)
}

func TestStartupReadsTokenFileAndRefusesInsecureMaster(t *testing.T) {
	dir:=t.TempDir()
	file:=filepath.Join(dir,"agent.token")
	if err:=os.WriteFile(file,[]byte("test-token-not-a-real-credential\n"),0600);err!=nil {t.Fatal(err)}
	cmd:=exec.Command(os.Args[0],"-test.run=^TestAgentStartupHelper$")
	cmd.Env=append(os.Environ(),"VIRTUALIS_STARTUP_HELPER=1","VIRTUALIS_STARTUP_TOKEN_FILE="+file,"VIRTUALIS_STARTUP_DATA="+dir)
	output,err:=cmd.CombinedOutput()
	if err==nil || !strings.Contains(string(output),"--allow-insecure") || strings.Contains(string(output),"test-token-not-a-real-credential") {t.Fatalf("startup must load token without exposing it and fail before insecure connection: err=%v out=%s",err,output)}
}
