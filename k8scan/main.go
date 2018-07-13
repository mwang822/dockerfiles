package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	mailgun "github.com/mailgun/mailgun-go"
)

const (
	defaultCIDR = "0.0.0.0/0"

	arinAPIEndpoint = "http://whois.arin.net/rest/ip/%s"

	emailSender = "k8scan@jessfraz.com"
)

var (
	timeoutPing time.Duration
	timeoutGet  time.Duration

	cidr string

	defaultPorts = intSlice{80, 443, 8001, 9001}
	ports        intSlice

	mailgunDomain  string
	mailgunAPIKey  string
	emailRecipient string

	debug bool
)

// intSlice is a slice of ints
type intSlice []int

// implement the flag interface for intSlice
func (i *intSlice) String() (out string) {
	for k, v := range *i {
		if k < len(*i)-1 {
			out += fmt.Sprintf("%d,", v)
		} else {
			out += fmt.Sprintf("%d", v)
		}
	}
	return out
}

func (i *intSlice) Set(value string) error {
	// Set the default if nothing was given.
	if len(value) <= 0 {
		*i = defaultPorts
		return nil
	}

	// Split on "," for individual ports and ranges.
	r := strings.Split(value, ",")
	for _, pr := range r {
		// Split on "-" to denote a range.
		if strings.Contains(pr, "-") {
			p := strings.SplitN(pr, "-", 2)
			begin, err := strconv.Atoi(p[0])
			if err != nil {
				return err
			}
			end, err := strconv.Atoi(p[1])
			if err != nil {
				return err
			}
			if begin > end {
				return fmt.Errorf("end port can not be greater than the beginning port: %d > %d", end, begin)
			}
			for port := begin; port <= end; port++ {
				*i = append(*i, port)
			}

			continue
		}

		// It is not a range just parse the port
		port, err := strconv.Atoi(pr)
		if err != nil {
			return err
		}
		*i = append(*i, port)
	}

	return nil
}

func init() {
	flag.DurationVar(&timeoutPing, "timeout-ping", 2*time.Second, "Timeout for checking that the port is open")
	flag.DurationVar(&timeoutGet, "timeout-get", 10*time.Second, "Timeout for getting the contents of the URL")

	flag.StringVar(&cidr, "cidr", defaultCIDR, "IP CIDR to scan")
	flag.Var(&ports, "ports", fmt.Sprintf("Ports to scan (ex. 80-443 or 80,443,8080 or 1-20,22,80-443) (default %q)", defaultPorts.String()))

	flag.StringVar(&mailgunAPIKey, "mailgun-api-key", "", "Mailgun API Key to use for sending email (optional)")
	flag.StringVar(&mailgunDomain, "mailgun-domain", "", "Mailgun Domain to use for sending email (optional)")
	flag.StringVar(&emailRecipient, "email-recipient", "", "Recipient for email notifications (optional)")

	flag.BoolVar(&debug, "d", false, "Run in debug mode")

	flag.Usage = func() {
		flag.PrintDefaults()
	}

	flag.Parse()

	// set log level
	if debug {
		logrus.SetLevel(logrus.DebugLevel)
	}
}

func main() {
	// On ^C, or SIGTERM handle exit.
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	signal.Notify(c, syscall.SIGTERM)
	go func() {
		for sig := range c {
			logrus.Infof("Received %s, exiting.", sig.String())
			os.Exit(0)
		}
	}()

	// Set the logger to nil so we ignore messages from the Dial that don't matter.
	// See: https://github.com/golang/go/issues/19895#issuecomment-292793756
	log.SetFlags(0)
	log.SetOutput(ioutil.Discard)

	logrus.Infof("Scanning for Kubernetes Dashboards and API Servers on %s over port range %#v", cidr, ports)
	if len(mailgunDomain) > 0 && len(mailgunAPIKey) > 0 && len(emailRecipient) > 0 {
		logrus.Infof("Using Mailgun Domain %s, API Key %s to send emails to %s", mailgunDomain, mailgunAPIKey, emailRecipient)
	}
	logrus.Infof("This may take a bit...")

	startTime := time.Now()

	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		logrus.Fatal(err)
	}

	var wg sync.WaitGroup
	for ip := ip.Mask(ipnet.Mask); ipnet.Contains(ip); inc(ip) {
		for _, port := range ports {
			wg.Add(1)
			go func(ip string, port int) {
				defer wg.Done()

				scanIP(ip, port)

			}(ip.String(), port)
		}
	}

	wg.Wait()

	since := time.Since(startTime)
	logrus.Infof("Scan took: %s", since.String())
}

func scanIP(ip string, port int) {
	// Check if the port is open.
	ok := portOpen(ip, port)
	if !ok {
		return
	}

	// Check if it's a kubernetes dashboard.
	ok, uri := isKubernetesDashboard(ip, port)
	if !ok {
		return
	}

	// Get the info for the ip address.
	info, err := getIPInfo(ip)
	if err != nil {
		logrus.Warnf("ip info err: %v", err)
	}

	fmt.Printf("%s:%d\t%s\t%s\t%s\n",
		ip, port,
		info.Net.Organization.Handle, info.Net.Organization.Name, info.Net.Organization.Reference)

	// send an email
	if len(mailgunDomain) > 0 && len(mailgunAPIKey) > 0 && len(emailRecipient) > 0 {
		if err := sendEmail(uri, ip, port, info); err != nil {
			logrus.Warn(err)
		}
	}
}

func portOpen(ip string, port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, port), timeoutPing)
	if err != nil {
		logrus.Debugf("listen at %s:%s failed: %v", ip, port, err)
		return false
	}
	if c != nil {
		c.Close()
	}

	return true
}

func isKubernetesDashboard(ip string, port int) (bool, string) {
	client := &http.Client{
		Timeout: timeoutGet,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	tryAddrs := []string{
		fmt.Sprintf("http://%s:%d", ip, port),
		fmt.Sprintf("https://%s:%d", ip, port),
		fmt.Sprintf("http://%s:%d/api/", ip, port),
		fmt.Sprintf("https://%s:%d/api/", ip, port),
	}

	var (
		resp *http.Response
		err  = errors.New("not yet run")
		uri  string
	)

	for i := 0; i < len(tryAddrs) && err != nil; i++ {
		uri = tryAddrs[i]
		resp, err = client.Get(uri)
	}
	if err != nil {
		logrus.Debugf("getting %s:%s failed: %v", ip, port, err)
		return false, ""
	}
	defer resp.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return false, ""
	}

	body := strings.ToLower(string(b))
	if (strings.Contains(body, "kubernetes") && strings.Contains(body, "dashboard")) ||
		(strings.Contains(body, `"versions"`) && strings.Contains(body, `"serverAddress`)) ||
		(strings.Contains(body, `"paths"`) && strings.Contains(body, `"/api"`)) {
		return true, uri
	}

	return false, ""
}

// ARINResponse describes the data struct that holds the response from ARIN.
type ARINResponse struct {
	Net NetJSON `json:"net,omitempty"`
}

// NetJSON holds the net data from the ARIN response.
type NetJSON struct {
	Organization OrganizationJSON `json:"orgRef,omitempty"`
}

// OrganizationJSON holds the organization data from the ARIN response.
type OrganizationJSON struct {
	Handle    string `json:"@handle,omitempty"`
	Name      string `json:"@name,omitempty"`
	Reference string `json:"$,omitempty"`
}

func getIPInfo(ip string) (b ARINResponse, err error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf(arinAPIEndpoint, ip), nil)
	if err != nil {
		return b, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return b, err
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return b, err
	}

	return b, nil
}

func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func sendEmail(uri, ip string, port int, arinInfo ARINResponse) error {
	mailgunClient := mailgun.NewMailgun(mailgunDomain, mailgunAPIKey, "")

	msg, _, err := mailgunClient.Send(mailgunClient.NewMessage(
		/* From */ fmt.Sprintf("%s <%s>", emailSender, emailSender),
		/* Subject */ fmt.Sprintf("[k8scan]: found dashboard %s", uri),
		/* Body */ fmt.Sprintf(`Time: %s

IP: %s:%d
URL: %s

ARIN: %s
	  %s
	  %s
`,
			time.Now().Format(time.UnixDate),
			ip,
			port,
			uri,
			arinInfo.Net.Organization.Handle,
			arinInfo.Net.Organization.Name,
			arinInfo.Net.Organization.Reference,
		),
		/* To */ emailRecipient,
	))
	if err != nil {
		return fmt.Errorf("sending Mailgun message failed: response: %#v error: %v", msg, err)
	}

	return nil
}
