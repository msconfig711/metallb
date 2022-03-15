// SPDX-License-Identifier:Apache-2.0

package frr

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"syscall"
	"text/template"
)

var configFileName = "/etc/frr_reloader/frr.conf"
var reloaderPidFileName = "/etc/frr_reloader/reloader.pid"

// TODO: We will probably need to update this template when we start to
// integrate with FRR. The current template is a reasonable first pass
// and we can improve it in later commits.
//
// It may be necessary to arrange this into multiple nested templates
// (https://pkg.go.dev/text/template#hdr-Nested_template_definitions), this
// should also be considered.
const configTemplate = `
log file /etc/frr/frr.log {{.Loglevel}}
log timestamp precision 3
hostname {{.Hostname}}
{{range $r := .Routers -}}
router bgp {{.MyASN}}
  no bgp ebgp-requires-policy
  no bgp network import-check
  no bgp default ipv4-unicast
{{ if .RouterId }}
  bgp router-id {{.RouterId}}
{{- end }}
{{range .Neighbors }}
  neighbor {{.Addr}} remote-as {{.ASN}}
  {{ if .Port -}}
  neighbor {{.Addr}} port {{.Port}}
  {{- end }}
  neighbor {{.Addr}} timers {{.KeepaliveTime}} {{.HoldTime}}
  {{ if .Password -}}
  neighbor {{.Addr}} password {{.Password}}
  {{- end }}
  {{ if .SrcAddr -}}
  neighbor {{.Addr}} update-source {{.SrcAddr}}
  {{- end }}
{{- end }}
{{range $n := .Neighbors -}}
{{- if eq (len .Advertisements) 0}}
  address-family ipv4 unicast
    neighbor {{$n.Addr}} activate
  exit-address-family
  address-family ipv6 unicast
    neighbor {{$n.Addr}} activate
  exit-address-family
{{- end}}
 
{{- range $index,$a := .Advertisements}}
  {{- if ne .Prepend 0}}
  access-list ip-match-{{$index}} permit {{.Prefix}}
  route-map set-as-path permit 10
    match ip address ip-match-{{$index}}
    {{- $val := Iterate .Prepend $r.MyASN }}
    set as-path prepend {{- $val}}
  route-map set-as-path permit 20
  {{- end}}

  address-family {{.Version}} unicast
    neighbor {{$n.Addr}} activate
    network {{.Prefix}}
    {{- if ne .Prepend 0}}
    neighbor {{$n.Addr}} route-map set-as-path out
    {{- end}}
  exit-address-family
{{- end}}
{{end}}
{{end}}
`

type frrConfig struct {
	Loglevel string
	Hostname string
	Routers  map[string]*routerConfig
}

type routerConfig struct {
	MyASN     uint32
	RouterId  string
	Neighbors map[string]*neighborConfig
}

type neighborConfig struct {
	ASN            uint32
	Addr           string
	SrcAddr        string
	Port           uint16
	HoldTime       uint64
	KeepaliveTime  uint64
	Password       string
	Advertisements []*advertisementConfig
}

type advertisementConfig struct {
	Version string
	Prefix  string
	Prepend uint32
}

// routerName() defines the format of the key of the "Routers" map in the
// frrConfig struct.
func routerName(srcAddr string, myASN uint32) string {
	return fmt.Sprintf("%d@%s", myASN, srcAddr)
}

// neighborName() defines the format of key of the 'Neighbors' map in the
// routerConfig struct.
func neighborName(peerAddr string, ASN uint32) string {
	return fmt.Sprintf("%d@%s", ASN, peerAddr)
}

// templateConfig uses the template library to template
// 'globalConfigTemplate' using 'data'.
func templateConfig(data interface{}) (string, error) {
	fm := template.FuncMap{
		"Iterate": func(count, as uint32) string {
			var i uint32
			var Items string
			for i = 0; i < (count); i++ {
				Items = fmt.Sprintf("%s %d", Items, as)
			}
			return Items
		},
	}
	t, err := template.New("FRR Config Template").Funcs(fm).Parse(configTemplate)
	if err != nil {
		return "", err
	}

	var b bytes.Buffer
	err = t.Execute(&b, data)

	return b.String(), err
}

// writeConfigFile writes the FRR configuration file (represented as a string)
// to 'filename'.
func writeConfig(config string, filename string) error {
	return ioutil.WriteFile(filename, []byte(config), 0644)
}

// reloadConfig requests that FRR reloads the configuration file. This is
// called after updating the configuration.
var reloadConfig = func() error {
	pidFile, found := os.LookupEnv("FRR_RELOADER_PID_FILE")
	if found {
		reloaderPidFileName = pidFile
	}

	pid, err := os.ReadFile(reloaderPidFileName)
	if err != nil {
		return err
	}

	pidInt, err := strconv.Atoi(string(pid))
	if err != nil {
		return err
	}

	// send HUP signal to FRR reloader
	err = syscall.Kill(pidInt, syscall.SIGHUP)
	if err != nil {
		return err
	}

	return nil
}

// generateAndReloadConfigFile takes a 'struct frrConfig' and, using a template,
// generates and writes a valid FRR configuration file. If this completes
// successfully it will also force FRR to reload that configuration file.
func generateAndReloadConfigFile(config *frrConfig) error {
	filename, found := os.LookupEnv("FRR_CONFIG_FILE")
	if found {
		configFileName = filename
	}

	configString, err := templateConfig(config)
	if err != nil {
		return err
	}

	err = writeConfig(configString, configFileName)
	if err != nil {
		return err
	}

	err = reloadConfig()
	if err != nil {
		return err
	}

	return nil
}
