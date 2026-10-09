package driver

import (
	"encoding/xml"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/SakuraOpenSource/virtualis-agent/internal/protocol"
)

func TestContainerVNCCommandRestrictsUnauthenticatedConsoleToLoopback(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "container_vnc.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found, local := false, false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		first, ok := call.Args[0].(*ast.BasicLit)
		if !ok || first.Value != `"x11vnc"` {
			return true
		}
		found = true
		for _, arg := range call.Args[1:] {
			if literal, ok := arg.(*ast.BasicLit); ok {
				value, _ := strconv.Unquote(literal.Value)
				local = local || value == "-localhost"
			}
		}
		return true
	})
	if !found || !local {
		t.Fatal("x11vnc must use -localhost; -nopw must never expose a root console on external interfaces")
	}
}

func TestQEMUVNCXMLRestrictsConsoleToLoopback(t *testing.T) {
	var domain struct {
		Devices struct {
			Graphics []struct {
				Type string `xml:"type,attr"`
				Listen string `xml:"listen,attr"`
			} `xml:"graphics"`
		} `xml:"devices"`
	}
	if err := xml.Unmarshal([]byte(domainXML("console", &protocol.Instance{}, "", "")), &domain); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, graphics := range domain.Devices.Graphics {
		if graphics.Type == "vnc" {
			found = true
			if graphics.Listen != "127.0.0.1" {
				t.Fatalf("VNC listens on %q, want 127.0.0.1", graphics.Listen)
			}
		}
	}
	if !found {
		t.Fatal("VNC console missing")
	}
}
