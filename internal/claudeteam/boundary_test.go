package claudeteam

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The host boundary must remain real: adding a dependency on the provider,
// inference implementation or UI would turn this back into a coupled fork.
func TestCoordinationDoesNotImportItsHosts(t *testing.T) {
	paths,err:=filepath.Glob("*.go");if err!=nil{t.Fatal(err)}
	for _,path:=range paths {
		if strings.HasSuffix(path,"_test.go"){continue}
		f,err:=parser.ParseFile(token.NewFileSet(),path,nil,parser.ImportsOnly);if err!=nil{t.Fatal(err)}
		ast.Inspect(f,func(n ast.Node)bool{
			i,ok:=n.(*ast.ImportSpec);if !ok{return true}
			name,_:=strconv.Unquote(i.Path.Value)
			for _,host:=range []string{"provider","gateway","gui","claudelogin","agent","routes"} {
				if name=="github.com/yetone/magpie/internal/"+host {t.Errorf("%s depends on host %s",path,host)}
			}
			return true
		})
	}
}
