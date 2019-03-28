package main

import (
	"fmt"
	"github.com/gswly/gomavlib"
	"gopkg.in/alecthomas/kingpin.v2"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

var reMsgName = regexp.MustCompile("^[A-Z0-9_]+$")
var reTypeIsArray = regexp.MustCompile("^(.+?)\\[([0-9]+)\\]$")

type outField struct {
	Line string
}

type outMessage struct {
	Name   string
	Id     int
	Fields []*outField
}

type outDefinition struct {
	Name     string
	Messages []*outMessage
	enums    []*DefinitionEnum
}

type outEnum struct {
	Name   string
	Values []*DefinitionEnumValue
}

var tpl = template.Must(template.New("").Parse(
	`// autogenerated with dialgen. do not edit.

package {{ .Name }}

import (
	"github.com/gswly/gomavlib"
)

// Version contains the dialect version. It is used in the mavlink_version field
// of the HEARTBEAT message.
var Version = {{.Version}}

// Dialect contains the dialect object that can be passed to the library.
var Dialect = dialect

var dialect = gomavlib.MustDialect([]gomavlib.Message{
{{- range .Defs }}
    // {{ .Name }}
{{- range .Messages }}
    &Message{{ .Name }}{},
{{- end }}
{{- end }}
})
{{/**/}}
{{- range .Defs }}
// {{ .Name }}
{{/**/}}
{{- range .Messages }}
type Message{{ .Name }} struct {
{{- range .Fields }}
    {{ .Line }}
{{- end }}
}

func (*Message{{ .Name }}) GetId() uint32 {
    return {{ .Id }}
}
{{ end }}
{{- end }}

{{- range .Enums }}
type {{ .Name }} int

const (
{{- $pn := .Name }}
{{- range .Values }}
	{{ .Name }} {{ $pn }} = {{ .Value }}
{{- end }}
)
{{ end }}
`))

func main() {
	kingpin.CommandLine.Help = "Generate a Mavlink dialect library from a definition file.\n" +
		"Example: dialgen \\\n--output=dialect.go \\\nhttps://raw.githubusercontent.com/mavlink/mavlink/master/message_definitions/v1.0/common.xml"
	outfile := kingpin.Flag("output", "output file").Required().String()
	mainDefAddr := kingpin.Arg("xml", "a path or url pointing to a Mavlink dialect definition in XML format").Required().String()
	kingpin.Parse()

	err := do(*outfile, *mainDefAddr)
	if err != nil {
		fmt.Printf("Error: %s\n", err)
		os.Exit(1)
	}
}

func do(outfile string, mainDefAddr string) error {
	if strings.HasSuffix(outfile, ".go") == false {
		return fmt.Errorf("output file must end with .go")
	}

	fmt.Printf("DIALECT %s\n", mainDefAddr)

	version := ""
	defsProcessed := make(map[string]struct{})
	isRemote := func() bool {
		_, err := url.ParseRequestURI(mainDefAddr)
		return err == nil
	}()

	// parse all definitions recursively
	outDefs, err := definitionProcess(&version, defsProcessed, isRemote, mainDefAddr)
	if err != nil {
		return err
	}

	// merge enums together
	enums := make(map[string]*outEnum)
	for _, def := range outDefs {
		for _, defEnum := range def.enums {
			if _, ok := enums[defEnum.Name]; !ok {
				enums[defEnum.Name] = &outEnum{
					Name: defEnum.Name,
				}
			}
			enum := enums[defEnum.Name]

			for _, v := range defEnum.Values {
				enum.Values = append(enum.Values, v)
			}
		}
	}

	// fill enum missing values
	for _, enum := range enums {
		nextVal := 0
		for _, v := range enum.Values {
			if v.Value != "" {
				nextVal, _ = strconv.Atoi(v.Value)
				nextVal++
			} else {
				v.Value = strconv.Itoa(nextVal)
				nextVal++
			}
		}
	}

	// create output folder
	dir, _ := filepath.Split(outfile)
	os.Mkdir(dir, 0755)

	// open file
	f, err := os.Create(outfile)
	if err != nil {
		return err
	}
	defer f.Close()

	// dump
	return tpl.Execute(f, map[string]interface{}{
		"Name": func() string {
			_, name := filepath.Split(mainDefAddr)
			return strings.TrimSuffix(name, ".xml")
		}(),
		"Version": func() int {
			ret, _ := strconv.Atoi(version)
			return ret
		}(),
		"Defs":  outDefs,
		"Enums": enums,
	})
}

func definitionProcess(version *string, defsProcessed map[string]struct{}, isRemote bool, defAddr string) ([]*outDefinition, error) {
	// skip already processed
	if _, ok := defsProcessed[defAddr]; ok {
		return nil, nil
	}
	defsProcessed[defAddr] = struct{}{}

	fmt.Printf("definition %s\n", defAddr)

	content, err := definitionGet(isRemote, defAddr)
	if err != nil {
		return nil, err
	}

	def, err := definitionDecode(content)
	if err != nil {
		return nil, fmt.Errorf("unable to decode: %s", err)
	}

	addrPath, addrName := filepath.Split(defAddr)

	outDef := &outDefinition{
		Name:  addrName,
		enums: def.Enums,
	}
	var outDefs []*outDefinition

	// version
	if def.Version != "" {
		if *version != "" && *version != def.Version {
			return nil, fmt.Errorf("version defined twice (%s and %s)", def.Version, *version)
		}
		*version = def.Version
	}

	// includes
	for _, inc := range def.Includes {
		// prepend url to remote address
		if isRemote == true {
			inc = addrPath + inc
		}
		subDefs, err := definitionProcess(version, defsProcessed, isRemote, inc)
		if err != nil {
			return nil, err
		}
		outDefs = append(outDefs, subDefs...)
	}

	// messages
	for _, msg := range def.Messages {
		outMsg, err := messageProcess(msg)
		if err != nil {
			return nil, err
		}
		outDef.Messages = append(outDef.Messages, outMsg)
	}

	outDefs = append(outDefs, outDef)
	return outDefs, nil
}

func definitionGet(isRemote bool, defAddr string) ([]byte, error) {
	if isRemote == true {
		byt, err := urlDownload(defAddr)
		if err != nil {
			return nil, fmt.Errorf("unable to download: %s", err)
		}
		return byt, nil
	}

	byt, err := ioutil.ReadFile(defAddr)
	if err != nil {
		return nil, fmt.Errorf("unable to open: %s", err)
	}
	return byt, nil
}

func urlDownload(desturl string) ([]byte, error) {
	res, err := http.Get(desturl)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad return code: %v", res.StatusCode)
	}

	byt, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	return byt, nil
}

func messageProcess(msg *DefinitionMessage) (*outMessage, error) {
	if m := reMsgName.FindStringSubmatch(msg.Name); m == nil {
		return nil, fmt.Errorf("unsupported message name: %s", msg.Name)
	}

	outMsg := &outMessage{
		Name: gomavlib.DialectMsgDefToGo(msg.Name),
		Id:   msg.Id,
	}

	for _, f := range msg.Fields {
		outField, err := fieldProcess(f)
		if err != nil {
			return nil, err
		}
		outMsg.Fields = append(outMsg.Fields, outField)
	}

	return outMsg, nil
}

func fieldProcess(field *DialectField) (*outField, error) {
	outF := &outField{}
	tags := make(map[string]string)

	newname := gomavlib.DialectFieldDefToGo(field.Name)

	// name conversion is not univoque: add tag
	if gomavlib.DialectFieldGoToDef(newname) != field.Name {
		tags["mavname"] = field.Name
	}

	outF.Line += newname

	typ := field.Type
	arrayLen := ""

	if typ == "uint8_t_mavlink_version" {
		typ = "uint8_t"
	}

	// string or array
	if matches := reTypeIsArray.FindStringSubmatch(typ); matches != nil {
		// string
		if matches[1] == "char" {
			tags["mavlen"] = matches[2]
			typ = "char"
			// array
		} else {
			arrayLen = matches[2]
			typ = matches[1]
		}
	}

	// extension
	if field.Extension == true {
		tags["mavext"] = "true"
	}

	typ = gomavlib.DialectTypeDefToGo(typ)
	if typ == "" {
		return nil, fmt.Errorf("unknown type: %s", typ)
	}

	outF.Line += " "
	if arrayLen != "" {
		outF.Line += "[" + arrayLen + "]"
	}
	outF.Line += typ

	if len(tags) > 0 {
		var tmp []string
		for k, v := range tags {
			tmp = append(tmp, fmt.Sprintf("%s:\"%s\"", k, v))
		}
		sort.Strings(tmp)
		outF.Line += " `" + strings.Join(tmp, " ") + "`"
	}
	return outF, nil
}
