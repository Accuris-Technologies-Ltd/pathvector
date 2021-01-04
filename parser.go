package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/kennygrant/sanitize"
	"github.com/pelletier/go-toml"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"
)

var release = "devel" // This is set by go build

// Peer contains all information specific to a single peer network
type Peer struct {
	Asn           uint     `yaml:"asn" toml:"ASN" json:"asn"`
	Type          string   `yaml:"type" toml:"Type" json:"type"`
	Prepends      uint     `yaml:"prepends" toml:"Prepends" json:"prepends"`
	LocalPref     uint     `yaml:"local-pref" toml:"LocalPref" json:"local-pref"`
	Multihop      bool     `yaml:"multihop" toml:"Multihop" json:"multihop"`
	Passive       bool     `yaml:"passive" toml:"Passive" json:"passive"`
	Disabled      bool     `yaml:"disabled" toml:"Disabled" json:"disabled"`
	Password      string   `yaml:"password" toml:"Password" json:"password"`
	Port          uint16   `yaml:"port" toml:"Port" json:"port"`
	PreImport     string   `yaml:"pre-import" toml:"PreImport" json:"pre-import"`
	PreExport     string   `yaml:"pre-export" toml:"PreExport" json:"pre-export"`
	NeighborIps   []string `yaml:"neighbors" toml:"Neighbors" json:"neighbors"`
	ImportLimit4  uint     `yaml:"import-limit4" toml:"ImportLimit4" json:"import-limit4"`
	ImportLimit6  uint     `yaml:"import-limit6" toml:"ImportLimit6" json:"import-limit6"`
	SkipFilter    bool     `yaml:"skip-filter" toml:"SkipFilter" json:"skip-filter"`
	RsClient      bool     `yaml:"rs-client" toml:"RSClient" json:"rs-client"`
	RrClient      bool     `yaml:"rr-client" toml:"RRClient" json:"rr-client"`
	Bfd           bool     `yaml:"bfd" toml:"BFD" json:"bfd"`
	SessionGlobal string   `yaml:"session-global" toml:"SessionGlobal" json:"SessionGlobal"`

	AsSet      string   `yaml:"-" toml:"-" json:"-"`
	QueryTime  string   `yaml:"-" toml:"-" json:"-"`
	Name       string   `yaml:"-" toml:"-" json:"-"`
	PrefixSet4 []string `yaml:"-" toml:"-" json:"-"`
	PrefixSet6 []string `yaml:"-" toml:"-" json:"-"`
}

// Config contains global configuration about this router and BCG instance
type Config struct {
	Asn          uint             `yaml:"asn" toml:"ASN" json:"asn"`
	RouterId     string           `yaml:"router-id" toml:"Router-ID" json:"router-id"`
	Prefixes     []string         `yaml:"prefixes" toml:"Prefixes" json:"prefixes"`
	Peers        map[string]*Peer `yaml:"peers" toml:"Peers" json:"peers"`
	IrrDb        string           `yaml:"irrdb" toml:"IRRDB" json:"irrdb"`
	RtrServer    string           `yaml:"rtr-server" toml:"RTR-Server" json:"rtr-server"`
	KeepFiltered bool             `yaml:"keep-filtered" toml:"KeepFiltered" json:"keep-filtered"`
	MergePaths   bool             `yaml:"merge-paths" toml:"MergePaths" json:"merge-paths"`
	PrefSrc4     string           `yaml:"pref-src4" toml:"PrefSrc4" json:"PrefSrc4"`
	PrefSrc6     string           `yaml:"pref-src6" toml:"PrefSrc6" json:"PrefSrc6"`

	OriginSet4 []string `yaml:"-" toml:"-" json:"-"`
	OriginSet6 []string `yaml:"-" toml:"-" json:"-"`
	Hostname   string   `yaml:"-" toml:"-" json:"-"`
}

// PeeringDbResponse contains the response from a PeeringDB query
type PeeringDbResponse struct {
	Data []PeeringDbData `json:"data"`
}

// PeeringDbData contains the actual data from PeeringDB response
type PeeringDbData struct {
	Name    string `json:"name"`
	AsSet   string `json:"irr_as_set"`
	MaxPfx4 uint   `json:"info_prefixes4"`
	MaxPfx6 uint   `json:"info_prefixes6"`
}

// Config struct passed to peer template
type PeerTemplate struct {
	Peer   Peer
	Config Config
}

// Flags
var (
	configFilename     = flag.String("config", "/etc/bcg/config.yml", "Configuration file in YAML, TOML, or JSON format")
	outputDirectory    = flag.String("output", "/etc/bird/", "Directory to write output files to")
	templatesDirectory = flag.String("templates", "/etc/bcg/templates/", "Templates directory")
	birdSocket         = flag.String("socket", "/run/bird/bird.ctl", "BIRD control socket")
	dryRun             = flag.Bool("dryrun", false, "Skip modifying BIRD config. This can be used to test that your config syntax is correct.")
	debug              = flag.Bool("debug", false, "Show debugging messages")
	uiFile             = flag.String("uifile", "/tmp/bcg-ui.html", "File to store web UI index page")
	noui               = flag.Bool("noui", false, "Disable generating web UI")
)

// Query PeeringDB for an ASN
func getPeeringDbData(asn uint) PeeringDbData {
	httpClient := http.Client{Timeout: time.Second * 5}
	req, err := http.NewRequest(http.MethodGet, "https://peeringdb.com/api/net?asn="+strconv.Itoa(int(asn)), nil)
	if err != nil {
		log.Fatalf("PeeringDB GET (This peer might not have a PeeringDB page): %v", err)
	}

	res, err := httpClient.Do(req)
	if err != nil {
		log.Fatalf("PeeringDB GET Request: %v", err)
	}

	if res.Body != nil {
		//noinspection GoUnhandledErrorResult
		defer res.Body.Close()
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		log.Fatalf("PeeringDB Read: %v", err)
	}

	var peeringDbResponse PeeringDbResponse
	if err := json.Unmarshal(body, &peeringDbResponse); err != nil {
		log.Fatalf("PeeringDB JSON Unmarshal: %v", err)
	}

	if len(peeringDbResponse.Data) < 1 {
		log.Fatalf("Peer %d doesn't have a valid PeeringDB entry. Try import-valid or ask the network to update their account.", asn)
	}

	return peeringDbResponse.Data[0]
}

// Use bgpq4 to generate a prefix filter and return only the filter lines
func getPrefixFilter(asSet string, family uint8, irrdb string) []string {
	// Run bgpq4 for BIRD format with aggregation enabled
	log.Infof("Running bgpq4 -h %s -Ab%d %s", irrdb, family, asSet)
	cmd := exec.Command("bgpq4", "-h", irrdb, "-Ab"+strconv.Itoa(int(family)), asSet)
	stdout, err := cmd.Output()
	if err != nil {
		log.Fatalf("bgpq4 error: %v", err.Error())
	}

	// Remove whitespace and commas from output
	output := strings.ReplaceAll(string(stdout), ",\n    ", "\n")

	// Remove array prefix
	output = strings.ReplaceAll(output, "NN = [\n    ", "")

	// Remove array suffix
	output = strings.ReplaceAll(output, "];", "")

	// Check for empty IRR
	if output == "" {
		log.Warnf("Peer with as-set %s has no IPv%d prefixes. Disabled IPv%d connectivity.", asSet, family, family)
		return []string{}
	}

	// Remove whitespace (in this case there should only be trailing whitespace)
	output = strings.TrimSpace(output)

	// Split output by newline
	return strings.Split(output, "\n")
}

// Nonbuffered io Reader
func readNoBuffer(reader io.Reader) string {
	buf := make([]byte, 1024)
	n, err := reader.Read(buf[:])

	if err != nil {
		log.Fatalf("BIRD read error: ", err)
	}

	return string(buf[:n])
}

// Run a bird command
func runBirdCommand(command string) {
	log.Println("Connecting to BIRD socket")
	conn, err := net.Dial("unix", *birdSocket)
	if err != nil {
		log.Fatalf("BIRD socket connect: %v", err)
	}
	//noinspection GoUnhandledErrorResult
	defer conn.Close()

	log.Println("Connected to BIRD socket")
	log.Printf("BIRD init response: %s", readNoBuffer(conn))

	log.Printf("Sending BIRD command: %s", command)
	_, err = conn.Write([]byte(strings.Trim(command, "\n") + "\n"))
	log.Printf("Sent BIRD command: %s", command)
	if err != nil {
		log.Fatalf("BIRD write error:", err)
	}

	log.Printf("BIRD response: %s", readNoBuffer(conn))
}

// Normalize a string to be filename-safe
func normalize(input string) string {
	// Remove non-alphanumeric characters
	input = sanitize.Path(input)

	// Make uppercase
	input = strings.ToUpper(input)

	// Replace spaces with underscores
	input = strings.ReplaceAll(input, " ", "_")

	// Replace slashes with dashes
	input = strings.ReplaceAll(input, "/", "-")

	return input
}

// Load a configuration file (YAML, JSON, or TOML)
func loadConfig() Config {
	configFile, err := ioutil.ReadFile(*configFilename)
	if err != nil {
		log.Fatalf("Reading %s: %v", *configFilename, err)
	}

	var config Config

	_splitFilename := strings.Split(*configFilename, ".")
	switch extension := _splitFilename[len(_splitFilename)-1]; extension {
	case "yaml", "yml":
		log.Info("Using YAML configuration format")
		err := yaml.Unmarshal(configFile, &config)
		if err != nil {
			log.Fatalf("YAML Unmarshal: %v", err)
		}
	case "toml":
		log.Info("Using TOML configuration format")
		err := toml.Unmarshal(configFile, &config)
		if err != nil {
			log.Fatalf("TOML Unmarshal: %v", err)
		}
	case "json":
		log.Info("Using JSON configuration format")
		err := json.Unmarshal(configFile, &config)
		if err != nil {
			log.Fatalf("JSON Unmarshal: %v", err)
		}
	default:
		log.Fatalf("Files with extension '%s' are not supported. (Acceptable values are yaml, toml, json", extension)
	}

	return config
}

func main() {
	// Enable debug logging in development releases
	if //noinspection GoBoolExpressions
	release == "devel" || *debug {
		log.SetLevel(log.DebugLevel)
	}

	flag.Usage = func() {
		fmt.Printf("Usage for bcg (%s) https://github.com/natesales/bcg:\n", release)
		flag.PrintDefaults()
	}
	flag.Parse()

	log.Infof("Starting BCG %s", release)

	funcMap := template.FuncMap{
		"Contains": func(s, substr string) bool {
			// String contains
			return strings.Contains(s, substr)
		},

		"Iterate": func(count *uint) []uint {
			// Create array with `count` entries
			var i uint
			var Items []uint
			for i = 0; i < (*count); i++ {
				Items = append(Items, i)
			}
			return Items
		},

		"BirdSet": func(filter []string) string {
			// Build a formatted BIRD prefix list
			output := ""
			for i, prefix := range filter {
				output += "    " + prefix
				if i != len(filter)-1 {
					output += ",\n"
				}
			}

			return output
		},

		"NotEmpty": func(arr []string) bool {
			// Is `arr` empty?
			return len(arr) != 0
		},

		"CheckProtocol": func(v4set []string, v6set []string, family string, peerType string) bool {
			if peerType == "downstream" || peerType == "peer" { // Only match IRR filtered peer types
				if family == "4" {
					return len(v4set) != 0
				} else {
					return len(v6set) != 0
				}
			} else { // If the peer type isn't going to be IRR filtered, ignore it.
				return true
			}
		},

		"CurrentTime": func() string {
			// get current timestamp
			return time.Now().Format(time.RFC1123)
		},
	}

	log.Debug("Loading templates")

	// Generate peer template
	peerTemplate, err := template.New("").Funcs(funcMap).ParseFiles(path.Join(*templatesDirectory, "peer.tmpl"))
	if err != nil {
		log.Fatalf("Read peer template: %v", err)
	}

	// Generate global template
	globalTemplate, err := template.New("").Funcs(funcMap).ParseFiles(path.Join(*templatesDirectory, "global.tmpl"))
	if err != nil {
		log.Fatalf("Read global template: %v", err)
	}

	// Generate UI template
	uiTemplate, err := template.New("").Funcs(funcMap).ParseFiles(path.Join(*templatesDirectory, "ui.tmpl"))
	if err != nil {
		log.Fatalf("Read ui template: %v", err)
	}

	log.Debug("Finished loading templates")

	// Load the config file from configFilename flag
	log.Debugf("Loading config from %s", *configFilename)
	config := loadConfig()
	log.Debug("Finished loading config")

	log.Debug("Linting global configuration")

	// Set default IRRDB
	if config.IrrDb == "" {
		config.IrrDb = "rr.ntt.net"
	}
	log.Infof("Using IRRDB server %s", config.IrrDb)

	// Set default RTR server
	if config.RtrServer == "" {
		config.RtrServer = "127.0.0.1"
	}
	log.Infof("Using RTR server %s", config.RtrServer)

	// Validate Router ID in dotted quad format
	if net.ParseIP(config.RouterId).To4() == nil {
		log.Fatalf("Router ID %s is not in valid dotted quad notation", config.RouterId)
	}

	// Validate CIDR notation of originated prefixes
	for _, addr := range config.Prefixes {
		if _, _, err := net.ParseCIDR(addr); err != nil {
			log.Fatalf("%s is not a valid IPv4 or IPv6 prefix in CIDR notation", addr)
		}
	}

	log.Debug("Finished linting global config")

	config.Hostname, err = os.Hostname()
	if err != nil {
		log.Warn("Unable to get hostname")
	}

	if len(config.Prefixes) == 0 {
		log.Info("There are no origin prefixes defined")
	} else {
		log.Debug("Building origin sets")

		// Assemble originIpv{4,6} lists by address family
		var originIpv4, originIpv6 []string
		for _, prefix := range config.Prefixes {
			if strings.Contains(prefix, ":") {
				originIpv6 = append(originIpv6, prefix)
			} else {
				originIpv4 = append(originIpv4, prefix)
			}
		}

		log.Debug("Finished building origin sets")

		log.Debug("OriginIpv4: ", originIpv4)
		log.Debug("OriginIpv6: ", originIpv6)

		config.OriginSet4 = originIpv4
		config.OriginSet6 = originIpv6
	}

	if !*dryRun {
		// Create the global output file
		log.Debug("Creating global config")
		globalFile, err := os.Create(path.Join(*outputDirectory, "bird.conf"))
		if err != nil {
			log.Fatalf("Create global BIRD output file: %v", err)
		}
		log.Debug("Finished creating global config file")

		// Render the global template and write to disk
		log.Debug("Writing global config file")
		err = globalTemplate.ExecuteTemplate(globalFile, "global.tmpl", config)
		if err != nil {
			log.Fatalf("Execute global template: %v", err)
		}
		log.Debug("Finished writing global config file")

		// Remove old peer-specific configs
		files, err := filepath.Glob(path.Join(*outputDirectory, "AS*.conf"))
		if err != nil {
			panic(err)
		}
		for _, f := range files {
			if err := os.Remove(f); err != nil {
				log.Fatalf("Removing old config files: %v", err)
			}
		}
	} else {
		log.Info("Dry run is enabled, skipped writing global config and removing old peer configs")
	}

	// Iterate over peers
	for peerName, peerData := range config.Peers {
		// Set peerName
		_peerName := strings.ReplaceAll(normalize(peerName), "-", "_")
		if unicode.IsDigit(rune(_peerName[0])) {
			_peerName = "PEER_" + _peerName
		}

		peerData.Name = _peerName

		// Set default query time
		peerData.QueryTime = "[No operations performed]"

		log.Infof("Checking config for %s AS%d", peerName, peerData.Asn)

		// Validate peer type
		if !(peerData.Type == "upstream" || peerData.Type == "peer" || peerData.Type == "downstream" || peerData.Type == "import-valid") {
			log.Fatalf("    type attribute is invalid. Must be upstream, peer, downstream, or import-valid", peerName)
		}

		log.Infof("    type: %s", peerData.Type)

		// Set default local pref
		if peerData.LocalPref == 0 {
			peerData.LocalPref = 100
		}

		// Only query PeeringDB and IRRDB for peers and downstreams
		if peerData.Type == "peer" || peerData.Type == "downstream" {
			peerData.QueryTime = time.Now().Format(time.RFC1123)
			peeringDbData := getPeeringDbData(peerData.Asn)

			if peerData.ImportLimit4 == 0 {
				peerData.ImportLimit4 = peeringDbData.MaxPfx4
				log.Infof("Peer %s has no IPv4 import limit configured. Setting to %d from PeeringDB", peerName, peeringDbData.MaxPfx4)
			}

			if peerData.ImportLimit6 == 0 {
				peerData.ImportLimit6 = peeringDbData.MaxPfx6
				log.Infof("Peer %s has no IPv6 import limit configured. Setting to %d from PeeringDB", peerName, peeringDbData.MaxPfx6)
			}

			if strings.Contains(peeringDbData.AsSet, "::") {
				peerData.AsSet = strings.Split(peeringDbData.AsSet, "::")[1]
			} else {
				peerData.AsSet = peeringDbData.AsSet
			}

			peerData.PrefixSet4 = getPrefixFilter(peerData.AsSet, 4, config.IrrDb)
			peerData.PrefixSet6 = getPrefixFilter(peerData.AsSet, 6, config.IrrDb)

			// Update the "latest operation" timestamp
			peerData.QueryTime = time.Now().Format(time.RFC1123)
		} else if peerData.Type == "upstream" || peerData.Type == "import-valid" {
			// Check if upstream has MaxPrefix4/6 set, if not set sensible defaults and if they are configured too low, warn the user
			if peerData.ImportLimit4 == 0 {
				peerData.ImportLimit4 = 1000000 // 1M routes
				log.Infof("Upstream/Import-Valid %s has no IPv4 import limit configured. Setting to 1,000,000", peerName)
			} else if peerData.ImportLimit4 <= 900000 {
				log.Infof("Upstream/Import-Valid %s has a low IPv4 import limit configured. You may want to increase the import limit.", peerName)
			}

			if peerData.ImportLimit6 == 0 {
				peerData.ImportLimit6 = 150000 // 150k routes
				log.Infof("Upstream/Import-Valid %s has no IPv6 import limit configured. Setting to 150,000", peerName)
			} else if peerData.ImportLimit6 <= 98000 {
				log.Infof("Upstream/Import-Valid %s has a low IPv6 import limit configured. You may want to increase the import limit.", peerName)
			}
		}

		log.Infof("    local pref: %d", peerData.LocalPref)
		log.Infof("    max prefixes: IPv4 %d, IPv6 %d", peerData.ImportLimit4, peerData.ImportLimit6)

		// Check for additional options
		if peerData.AsSet != "" {
			log.Infof("    as-set: %s", peerData.AsSet)
		}

		if peerData.Prepends > 0 {
			log.Infof("    prepends: %d", peerData.Prepends)
		}

		if peerData.Multihop {
			log.Infof("    multihop")
		}

		if peerData.Passive {
			log.Infof("    passive")
		}

		if peerData.Disabled {
			log.Infof("    disabled")
		}

		if peerData.PreImport != "" {
			log.Infof("    pre-import: %s", peerData.PreImport)
		}

		if peerData.PreExport != "" {
			log.Infof("    pre-export: %s", peerData.PreExport)
		}

		// Log neighbor IPs
		log.Infof("    neighbors:")
		for _, ip := range peerData.NeighborIps {
			log.Infof("      %s", ip)
		}

		if !*dryRun {
			// Create the peer specific file
			peerSpecificFile, err := os.Create(path.Join(*outputDirectory, "AS"+strconv.Itoa(int(peerData.Asn))+"_"+normalize(peerName)+".conf"))
			if err != nil {
				log.Fatalf("Create peer specific output file: %v", err)
			}

			// Render the template and write to disk
			err = peerTemplate.ExecuteTemplate(peerSpecificFile, "peer.tmpl", &PeerTemplate{*peerData, config})
			if err != nil {
				log.Fatalf("Execute template: %v", err)
			}

			log.Infof("Wrote peer specific config for AS%d", peerData.Asn)
		} else {
			log.Infof("Dry run is enabled, skipped writing peer config(s)")
		}
	}

	if !*dryRun {
		if !*noui {
			// Create the ui output file
			log.Debug("Creating global config")
			uiFileObj, err := os.Create(*uiFile)
			if err != nil {
				log.Fatalf("Create UI output file: %v", err)
			}
			log.Debug("Finished creating UI file")

			// Render the UI template and write to disk
			log.Debug("Writing ui file")
			err = uiTemplate.ExecuteTemplate(uiFileObj, "ui.tmpl", config)
			if err != nil {
				log.Fatalf("Execute ui template: %v", err)
			}
			log.Debug("Finished writing ui file")
		}

		runBirdCommand("configure")
	}
}
