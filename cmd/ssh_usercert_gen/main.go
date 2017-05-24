package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"github.com/Symantec/Dominator/lib/logbuf"
	"github.com/Symantec/keymaster/lib/authutil"
	"github.com/Symantec/keymaster/lib/certgen"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tstranex/u2f"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v2"
	//"io"
	"io/ioutil"
	"log"
	//"net"
	"net/http"
	//"net/url"
	"os"
	"path/filepath"
	"regexp"
	//"strconv"
	"strings"
	"sync"
	"time"
)

// describes the network config and the mechanism for user auth.
// While the contents of the certificaes are public, we want to
// restrict generation to authenticated users
type baseConfig struct {
	HttpAddress      string `yaml:"http_address"`
	TLSCertFilename  string `yaml:"tls_cert_filename"`
	TLSKeyFilename   string `yaml:"tls_key_filename"`
	UserAuth         string
	SSHCAFilename    string `yaml:"ssh_ca_filename"`
	HtpasswdFilename string `yaml:"htpasswd_filename"`
	ClientCAFilename string `yaml:"client_ca_filename"`
	HostIdentity     string `yaml:"host_identity"`
	KerberosRealm    string `yaml:"kerberos_realm"`
	DataDirectory    string `yaml:"data_directory"`
}

type LdapConfig struct {
	Bind_Pattern     string
	LDAP_Target_URLs string
}

type AppConfigFile struct {
	Base baseConfig
	Ldap LdapConfig
}

type authInfo struct {
	ExpiresAt time.Time
	Username  string
}

type u2fAuthData struct {
	Counter      uint32
	Registration *u2f.Registration
}

type userProfile struct {
	U2fAuthData           []u2fAuthData
	RegistrationChallenge *u2f.Challenge
	u2fAuthChallenge      *u2f.Challenge
}

type RuntimeState struct {
	Config              AppConfigFile
	SSHCARawFileContent []byte
	Signer              crypto.Signer
	ClientCAPool        *x509.CertPool
	HostIdentity        string
	KerberosRealm       *string
	caCertDer           []byte
	authCookie          map[string]authInfo
	Mutex               sync.Mutex
	userProfile         map[string]userProfile
}

const userProfileFilename = "userProfiles.gob"

var (
	Version          = "No version provided"
	configFilename   = flag.String("config", "config.yml", "The filename of the configuration")
	debug            = flag.Bool("debug", false, "Enable debug messages to console")
	u2fAppID         = "https://www.example.com:33443"
	u2fTrustedFacets = []string{}
)

func getHostIdentity() (string, error) {
	return os.Hostname()
}

func exitsAndCanRead(fileName string, description string) ([]byte, error) {
	if _, err := os.Stat(fileName); os.IsNotExist(err) {
		return nil, err
	}
	buffer, err := ioutil.ReadFile(fileName)
	if err != nil {
		err = errors.New("cannot read " + description + "file")
		return nil, err
	}
	return buffer, err
}

func getSignerFromPEMBytes(privateKey []byte) (crypto.Signer, error) {
	return certgen.GetSignerFromPEMBytes(privateKey)
}

// Assumes the runtime state signer has been loaded!
func generateCADer(state *RuntimeState, keySigner crypto.Signer) ([]byte, error) {
	organizationName := state.HostIdentity
	if state.KerberosRealm != nil {
		organizationName = *state.KerberosRealm
	}
	return certgen.GenSelfSignedCACert(state.HostIdentity, organizationName, keySigner)
}

func (state *RuntimeState) performStateCleanup() {
	secsBetweenCleanup := 30
	for {
		state.Mutex.Lock()
		initAuthSize := len(state.authCookie)
		for key, authInfo := range state.authCookie {
			if authInfo.ExpiresAt.Before(time.Now()) {
				delete(state.authCookie, key)
			}
		}
		finalAuthSize := len(state.authCookie)
		state.Mutex.Unlock()
		if *debug {
			log.Printf("Auth Cookie sizes: before:(%d) after (%d)\n", initAuthSize, finalAuthSize)
		}
		time.Sleep(time.Duration(secsBetweenCleanup) * time.Second)
	}

}

func loadVerifyConfigFile(configFilename string) (RuntimeState, error) {
	var runtimeState RuntimeState
	if _, err := os.Stat(configFilename); os.IsNotExist(err) {
		err = errors.New("mising config file failure")
		return runtimeState, err
	}
	source, err := ioutil.ReadFile(configFilename)
	if err != nil {
		err = errors.New("cannot read config file")
		return runtimeState, err
	}
	err = yaml.Unmarshal(source, &runtimeState.Config)
	if err != nil {
		err = errors.New("Cannot parse config file")
		return runtimeState, err
	}

	//share config
	runtimeState.authCookie = make(map[string]authInfo)
	runtimeState.userProfile = make(map[string]userProfile)

	//verify config
	if len(runtimeState.Config.Base.HostIdentity) > 0 {
		runtimeState.HostIdentity = runtimeState.Config.Base.HostIdentity
	} else {
		runtimeState.HostIdentity, err = getHostIdentity()
		if err != nil {
			return runtimeState, err
		}
	}
	// TODO:HACK ALERT
	u2fAppID = "https://" + runtimeState.HostIdentity + ":33443"
	u2fTrustedFacets = append(u2fTrustedFacets, u2fAppID)

	if len(runtimeState.Config.Base.KerberosRealm) > 0 {
		runtimeState.KerberosRealm = &runtimeState.Config.Base.KerberosRealm
	}

	_, err = exitsAndCanRead(runtimeState.Config.Base.TLSCertFilename, "http cert file")
	if err != nil {
		return runtimeState, err
	}
	_, err = exitsAndCanRead(runtimeState.Config.Base.TLSKeyFilename, "http key file")
	if err != nil {
		return runtimeState, err
	}

	sshCAFilename := runtimeState.Config.Base.SSHCAFilename
	runtimeState.SSHCARawFileContent, err = exitsAndCanRead(sshCAFilename, "ssh CA File")
	if err != nil {
		log.Printf("Cannot load ssh CA File")
		return runtimeState, err
	}

	if len(runtimeState.Config.Base.ClientCAFilename) > 0 {
		clientCAbuffer, err := exitsAndCanRead(runtimeState.Config.Base.ClientCAFilename, "client CA file")
		if err != nil {
			log.Printf("Cannot load client CA File")
			return runtimeState, err
		}
		runtimeState.ClientCAPool = x509.NewCertPool()
		ok := runtimeState.ClientCAPool.AppendCertsFromPEM(clientCAbuffer)
		if !ok {
			err = errors.New("Cannot append any certs from Client CA file")
			return runtimeState, err
		}
		if *debug || true {
			log.Printf("client ca file loaded")
		}

	}
	if strings.HasPrefix(string(runtimeState.SSHCARawFileContent[:]), "-----BEGIN RSA PRIVATE KEY-----") {
		signer, err := getSignerFromPEMBytes(runtimeState.SSHCARawFileContent)
		if err != nil {
			log.Printf("Cannot parse Priave Key file")
			return runtimeState, err
		}
		runtimeState.caCertDer, err = generateCADer(&runtimeState, signer)
		if err != nil {
			log.Printf("Cannot generate CA Der")
			return runtimeState, err
		}

		// Assignmet of signer MUST be the last operation after
		// all error checks
		runtimeState.Signer = signer

	} else {
		if runtimeState.ClientCAPool == nil {
			err := errors.New("Invalid ssh CA private key file and NO clientCA")
			return runtimeState, err
		}
		//check that the loaded date seems like an openpgp armored file
		fileAsString := string(runtimeState.SSHCARawFileContent[:])
		if !strings.HasPrefix(fileAsString, "-----BEGIN PGP MESSAGE-----") {
			err = errors.New("Have a client CA but the CA file does NOT look like and PGP file")
			return runtimeState, err
		}

	}
	///
	err = runtimeState.LoadUserProfiles()
	if err != nil {
		log.Printf("Cannot load user Profile %s", err)
	}
	log.Printf("%+v", runtimeState.userProfile)

	// and we start the cleanup
	go runtimeState.performStateCleanup()

	return runtimeState, nil
}

func convertToBindDN(username string, bind_pattern string) string {
	return fmt.Sprintf(bind_pattern, username)
}

func checkUserPassword(username string, password string, config AppConfigFile) (bool, error) {
	//if username == "camilo_viecco1" && password == "pass" {
	//	return true, nil
	//}

	const timeoutSecs = 3
	bindDN := convertToBindDN(username, config.Ldap.Bind_Pattern)
	for _, ldapUrl := range strings.Split(config.Ldap.LDAP_Target_URLs, ",") {
		if len(ldapUrl) < 1 {
			continue
		}
		u, err := authutil.ParseLDAPURL(ldapUrl)
		if err != nil {
			log.Printf("Failed to parse ldapurl '%s'", ldapUrl)
			continue
		}
		vaild, err := authutil.CheckLDAPUserPassword(*u, bindDN, password, timeoutSecs, nil)
		if err != nil {
			//log.Printf("Failed to parse %s", ldapUrl)
			continue
		}
		// the ldap exchange was successful (user might be invaid)
		return vaild, nil

	}
	if config.Base.HtpasswdFilename != "" {
		if *debug {
			log.Printf("I have htpasswed filename")
		}
		buffer, err := ioutil.ReadFile(config.Base.HtpasswdFilename)
		if err != nil {
			return false, err
		}
		valid, err := authutil.CheckHtpasswdUserPassword(username, password, buffer)
		if err != nil {
			return false, err
		}
		return valid, nil
	}
	return false, nil
}

// returns application/json or text/html depending on the request. By default we assume the requester wants json
func getPreferredAcceptType(r *http.Request) string {
	preferredAcceptType := "application/json"
	acceptHeader, ok := r.Header["Accept"]
	if ok {
		for _, acceptValue := range acceptHeader {
			if strings.Contains(acceptValue, "text/html") {
				log.Printf("Got it  %+v", acceptValue)
				preferredAcceptType = "text/html"
			}
		}
	}
	return preferredAcceptType
}

func writeFailureResponse(w http.ResponseWriter, r *http.Request, code int, message string) {
	returnAcceptType := getPreferredAcceptType(r)
	if code == http.StatusUnauthorized && returnAcceptType != "text/html" {
		w.Header().Set("WWW-Authenticate", `Basic realm="User Credentials"`)
	}
	w.WriteHeader(code)
	publicErrorText := fmt.Sprintf("%d %s %s\n", code, http.StatusText(code), message)
	switch code {

	case http.StatusUnauthorized:
		switch returnAcceptType {
		case "text/html":
			// TODO: change by a message followed by an HTTP redirection
			fmt.Fprintf(w, "%s", loginFormText)
		default:
			w.Write([]byte(publicErrorText))
		}
	default:
		w.Write([]byte(publicErrorText))
	}
}

// Inspired by http://stackoverflow.com/questions/21936332/idiomatic-way-of-requiring-http-basic-auth-in-go
func checkAuth(w http.ResponseWriter, r *http.Request, state *RuntimeState) (string, error) {
	// We first check for cookies
	var authCookie *http.Cookie
	for _, cookie := range r.Cookies() {
		if cookie.Name != authCookieName {
			continue
		}
		authCookie = cookie
	}
	if authCookie == nil {
		//For now try also http basic (to be deprecated)
		user, pass, ok := r.BasicAuth()
		if !ok {
			writeFailureResponse(w, r, http.StatusUnauthorized, "")
			//toLoginOrBasicAuth(w, r)
			err := errors.New("check_Auth, Invalid or no auth header")
			return "", err
		}
		state.Mutex.Lock()
		config := state.Config
		state.Mutex.Unlock()
		valid, err := checkUserPassword(user, pass, config)
		if err != nil {
			writeFailureResponse(w, r, http.StatusInternalServerError, "")
			return "", err
		}
		if !valid {
			writeFailureResponse(w, r, http.StatusUnauthorized, "")
			err := errors.New("Invalid Credentials")
			return "", err
		}
		return user, nil
	}

	//Critical section
	state.Mutex.Lock()
	info, ok := state.authCookie[authCookie.Value]
	state.Mutex.Unlock()

	if !ok {
		//redirect to login page?
		//better would be to return the content of the redirect form with a 401 code?
		writeFailureResponse(w, r, http.StatusUnauthorized, "")
		err := errors.New("Invalid Cookie")
		return "", err
	}
	//check for expiration...
	if info.ExpiresAt.Before(time.Now()) {
		writeFailureResponse(w, r, http.StatusUnauthorized, "")
		err := errors.New("Expired Cookie")
		return "", err

	}

	return info.Username, nil

}

func (state *RuntimeState) SaveUserProfiles() error {
	var gobBuffer bytes.Buffer
	encoder := gob.NewEncoder(&gobBuffer)
	encoder.Encode(state.userProfile)
	userProfilePath := filepath.Join(state.Config.Base.DataDirectory, userProfileFilename)
	return ioutil.WriteFile(userProfilePath, gobBuffer.Bytes(), 0640)
}

func (state *RuntimeState) LoadUserProfiles() error {
	userProfilePath := filepath.Join(state.Config.Base.DataDirectory, userProfileFilename)

	fileBytes, err := exitsAndCanRead(userProfilePath, "user Profile file")
	if err != nil {
		log.Printf("problem with user Profile data")
		return err
	}
	gobReader := bytes.NewReader(fileBytes)
	decoder := gob.NewDecoder(gobReader)
	return decoder.Decode(&state.userProfile)
}

const CERTGEN_PATH = "/certgen/"

func (state *RuntimeState) certGenHandler(w http.ResponseWriter, r *http.Request) {
	var signerIsNull bool
	var keySigner crypto.Signer

	// copy runtime singer if not nil
	state.Mutex.Lock()
	signerIsNull = (state.Signer == nil)
	if !signerIsNull {
		keySigner = state.Signer
	}
	state.Mutex.Unlock()

	//local sanity tests
	if signerIsNull {
		writeFailureResponse(w, r, http.StatusInternalServerError, "")
		log.Printf("Signer not loaded")
		return
	}
	/*
	 */
	// TODO(camilo_viecco1): reorder checks so that simple checks are done before checking user creds
	authUser, err := checkAuth(w, r, state)
	if err != nil {
		log.Printf("%v", err)

		return
	}

	targetUser := r.URL.Path[len(CERTGEN_PATH):]
	if authUser != targetUser {
		writeFailureResponse(w, r, http.StatusForbidden, "")
		log.Printf("User %s asking for creds for %s", authUser, targetUser)
		return
	}
	if *debug {
		log.Printf("auth succedded for %s", authUser)
	}

	switch r.Method {
	case "GET":
		if *debug {
			log.Printf("Got client GET connection")
		}
		err = r.ParseForm()
		if err != nil {
			log.Println(err)
			writeFailureResponse(w, r, http.StatusBadRequest, "Error parsing form")
			return
		}
	case "POST":
		if *debug {
			log.Printf("Got client POST connection")
		}
		err = r.ParseMultipartForm(1e7)
		if err != nil {
			log.Println(err)
			writeFailureResponse(w, r, http.StatusBadRequest, "Error parsing form")
			return
		}
	default:
		writeFailureResponse(w, r, http.StatusMethodNotAllowed, "")
		return
	}

	certType := "ssh"
	if val, ok := r.Form["type"]; ok {
		certType = val[0]
	}
	log.Printf("cert type =%s", certType)

	switch certType {
	case "ssh":
		state.postAuthSSHCertHandler(w, r, targetUser, keySigner)
		return
	case "x509":
		state.postAuthX509CertHandler(w, r, targetUser, keySigner)
		return
	default:
		writeFailureResponse(w, r, http.StatusBadRequest, "Unrecognized cert type")
		return
	}
	//SHOULD have never reached this!
	writeFailureResponse(w, r, http.StatusInternalServerError, "")
	log.Printf("Escape from default paths")
	return

}

func (state *RuntimeState) postAuthSSHCertHandler(w http.ResponseWriter, r *http.Request, targetUser string, keySigner crypto.Signer) {
	signer, err := ssh.NewSignerFromSigner(keySigner)
	if err != nil {
		writeFailureResponse(w, r, http.StatusInternalServerError, "")
		log.Printf("Signer failed to load")
		return
	}

	var cert string
	switch r.Method {
	case "GET":
		cert, err = certgen.GenSSHCertFileStringFromSSSDPublicKey(targetUser, signer, state.HostIdentity)
		if err != nil {
			http.NotFound(w, r)
			return
		}
	case "POST":
		file, _, err := r.FormFile("pubkeyfile")
		if err != nil {
			log.Println(err)
			writeFailureResponse(w, r, http.StatusBadRequest, "Missing public key file")
			return
		}
		defer file.Close()
		buf := new(bytes.Buffer)
		buf.ReadFrom(file)
		userPubKey := buf.String()
		//validKey, err := regexp.MatchString("^(ssh-rsa|ssh-dss|ecdsa-sha2-nistp256|ssh-ed25519) [a-zA-Z0-9/+]+=?=? .*$", userPubKey)
		validKey, err := regexp.MatchString("^(ssh-rsa|ssh-dss|ecdsa-sha2-nistp256|ssh-ed25519) [a-zA-Z0-9/+]+=?=? ?.{0,512}\n?$", userPubKey)
		if err != nil {
			log.Println(err)
			writeFailureResponse(w, r, http.StatusInternalServerError, "")
			return
		}
		if !validKey {
			writeFailureResponse(w, r, http.StatusBadRequest, "Invalid File, bad re")
			log.Printf("invalid file, bad re")
			return

		}

		cert, err = certgen.GenSSHCertFileString(targetUser, userPubKey, signer, state.HostIdentity)
		if err != nil {
			writeFailureResponse(w, r, http.StatusInternalServerError, "")
			log.Printf("signUserPubkey Err")
			return
		}

	default:
		writeFailureResponse(w, r, http.StatusMethodNotAllowed, "")
		return

	}
	w.Header().Set("Content-Disposition", `attachment; filename="id_rsa-cert.pub"`)
	w.WriteHeader(200)
	fmt.Fprintf(w, "%s", cert)
	log.Printf("Generated SSH Certifcate for %s", targetUser)

}

func (state *RuntimeState) postAuthX509CertHandler(w http.ResponseWriter, r *http.Request, targetUser string, keySigner crypto.Signer) {
	var cert string
	switch r.Method {
	case "POST":
		file, _, err := r.FormFile("pubkeyfile")
		if err != nil {
			log.Println(err)
			writeFailureResponse(w, r, http.StatusBadRequest, "Missing public key file")
			return
		}
		defer file.Close()
		buf := new(bytes.Buffer)
		buf.ReadFrom(file)

		block, _ := pem.Decode(buf.Bytes())
		if block == nil || block.Type != "PUBLIC KEY" {
			writeFailureResponse(w, r, http.StatusBadRequest, "Invalid File, Unable to decode pem")
			log.Printf("invalid file, unable to decode pem")
			return
		}
		userPub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			writeFailureResponse(w, r, http.StatusBadRequest, "Cannot parse public key")
			log.Printf("Cannot parse public key")
			return
		}
		//tate.caCertDer
		caCert, err := x509.ParseCertificate(state.caCertDer)
		if err != nil {
			//writeFailureResponse(w, http.StatusBadRequest, "Cannot parse public key")
			writeFailureResponse(w, r, http.StatusInternalServerError, "")
			log.Printf("Cannot parse CA Der data")
			return
		}
		derCert, err := certgen.GenUserX509Cert(targetUser, userPub, caCert, keySigner, state.KerberosRealm)
		if err != nil {
			writeFailureResponse(w, r, http.StatusInternalServerError, "")
			log.Printf("Cannot Generate x509cert")
			return
		}
		cert = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derCert}))

	default:
		writeFailureResponse(w, r, http.StatusMethodNotAllowed, "")
		return

	}
	w.Header().Set("Content-Disposition", `attachment; filename="userCert.pem"`)
	w.WriteHeader(200)
	fmt.Fprintf(w, "%s", cert)
	log.Printf("Generated x509 Certifcate for %s", targetUser)
}

const SECRETINJECTOR_PATH = "/admin/inject"

func (state *RuntimeState) secretInjectorHandler(w http.ResponseWriter, r *http.Request) {
	// checks this is only allowed when using TLS client certs.. all other authn
	// mechanisms are considered invalid... for now no authz mechanisms are in place ie
	// Any user with a valid cert can use this handler
	if r.TLS == nil {
		writeFailureResponse(w, r, http.StatusInternalServerError, "")
		log.Printf("We require TLS\n")
		return
	}

	if len(r.TLS.VerifiedChains) < 1 {
		writeFailureResponse(w, r, http.StatusForbidden, "")
		log.Printf("Forbidden\n")
		return
	}
	clientName := r.TLS.VerifiedChains[0][0].Subject.CommonName
	log.Printf("Got connection from %s", clientName)
	r.ParseForm()
	sshCAPassword, ok := r.Form["ssh_ca_password"]
	if !ok {
		writeFailureResponse(w, r, http.StatusBadRequest, "Invalid Post, missing data")
		log.Printf("missing ssh_ca_password")
		return
	}
	state.Mutex.Lock()
	defer state.Mutex.Unlock()

	// TODO.. make network error blocks to goroutines
	if state.Signer != nil {
		writeFailureResponse(w, r, http.StatusConflict, "Conflict post, signer already unlocked")
		log.Printf("Signer not null, already unlocked")
		return
	}

	decbuf := bytes.NewBuffer(state.SSHCARawFileContent)

	armorBlock, err := armor.Decode(decbuf)
	if err != nil {
		log.Printf("Cannot decode armored file")
		return
	}
	password := []byte(sshCAPassword[0])
	failed := false
	prompt := func(keys []openpgp.Key, symmetric bool) ([]byte, error) {
		// If the given passphrase isn't correct, the function will be called again, forever.
		// This method will fail fast.
		// Ref: https://godoc.org/golang.org/x/crypto/openpgp#PromptFunction
		if failed {
			return nil, errors.New("decryption failed")
		}
		failed = true
		return password, nil
	}
	md, err := openpgp.ReadMessage(armorBlock.Body, nil, prompt, nil)
	if err != nil {
		log.Printf("cannot read message")
		return
	}

	plaintextBytes, err := ioutil.ReadAll(md.UnverifiedBody)
	if err != nil {
		return
	}

	signer, err := getSignerFromPEMBytes(plaintextBytes)
	if err != nil {
		log.Printf("Cannot parse Priave Key file")
		return
	}

	log.Printf("About to generate cader %s", clientName)
	state.caCertDer, err = generateCADer(state, signer)
	if err != nil {
		log.Printf("Cannot generate CA Der")
		return
	}

	// Assignmet of signer MUST be the last operation after
	// all error checks
	state.Signer = signer

	// TODO... make success a goroutine
	w.WriteHeader(200)
	fmt.Fprintf(w, "OK\n")
	//fmt.Fprintf(w, "%+v\n", r.TLS)
}

const PUBLIC_PATH = "/public/"

//Should be a template
const loginFormText = `
<html>
    <head>
	<meta charset="UTF-8">
	<title>{{.Title}}</title>
    </head>
    <body>
	<form enctype="application/x-www-form-urlencoded" action="/api/v0/login" method="post">
	    <p>Username: <INPUT TYPE="text" NAME="username" SIZE=18></p>
	    <p>Password: <INPUT TYPE="password" NAME="password" SIZE=18></p>
	    <p><input type="submit" value="Submit" /></p>
	</form>
    </body>
</html>
`

const loginFormPath = "/public/loginForm"

func (state *RuntimeState) publicPathHandler(w http.ResponseWriter, r *http.Request) {
	var signerIsNull bool

	// check if initialized(singer  not nil)
	state.Mutex.Lock()
	signerIsNull = (state.Signer == nil)
	state.Mutex.Unlock()
	if signerIsNull {
		writeFailureResponse(w, r, http.StatusInternalServerError, "")
		log.Printf("Signer not loaded")
		return
	}

	target := r.URL.Path[len(PUBLIC_PATH):]

	switch target {
	case "loginForm":
		w.WriteHeader(200)
		fmt.Fprintf(w, "%s", loginFormText)
		return
	case "x509ca":
		pemCert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: state.caCertDer}))

		w.Header().Set("Content-Disposition", `attachment; filename="id_rsa-cert.pub"`)
		w.WriteHeader(200)
		fmt.Fprintf(w, "%s", pemCert)
	default:
		writeFailureResponse(w, r, http.StatusNotFound, "")
		return
		//w.WriteHeader(200)
		//fmt.Fprintf(w, "OK\n")
	}
}

const authCookieName = "auth_cookie"
const randomStringEntropyBytes = 32
const maxAgeSecondsAuthCookie = 300

func genRandomString() (string, error) {
	size := randomStringEntropyBytes
	rb := make([]byte, size)
	_, err := rand.Read(rb)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(rb), nil
}

const LOGIN_PATH = "/api/v0/login"

func (state *RuntimeState) loginHandler(w http.ResponseWriter, r *http.Request) {
	//log.Printf("Top of LoginHandler")
	signerIsNull := true
	// check if initialized(singer  not nil)
	state.Mutex.Lock()
	signerIsNull = (state.Signer == nil)
	state.Mutex.Unlock()
	if signerIsNull {
		writeFailureResponse(w, r, http.StatusInternalServerError, "")
		log.Printf("Signer not loaded")
		return
	}

	//Check for valid method here?
	switch r.Method {
	case "GET":
		if *debug {
			log.Printf("Got client GET connection")
		}
		err := r.ParseForm()
		if err != nil {
			log.Println(err)
			writeFailureResponse(w, r, http.StatusBadRequest, "Error parsing form")
			return
		}
	case "POST":
		if *debug {
			log.Printf("Got client POST connection")
		}
		//err := r.ParseMultipartForm(1e7)
		err := r.ParseForm()
		if err != nil {
			log.Println(err)
			writeFailureResponse(w, r, http.StatusBadRequest, "Error parsing form")
			return
		}
		//log.Printf("req =%+v", r)
	default:
		writeFailureResponse(w, r, http.StatusMethodNotAllowed, "")
		return
	}

	//First headers and then check form
	username, password, ok := r.BasicAuth()
	if !ok {
		//var username string
		if val, ok := r.Form["username"]; ok {
			if len(val) > 1 {
				writeFailureResponse(w, r, http.StatusBadRequest, "Just one username allowed")
				log.Printf("Login with multiple usernames")
				return
			}
			username = val[0]
		}
		//var password string
		if val, ok := r.Form["password"]; ok {
			if len(val) > 1 {
				writeFailureResponse(w, r, http.StatusBadRequest, "Just one password allowed")
				log.Printf("Login with passwords")
				return
			}
			password = val[0]
		}

		if len(username) < 1 || len(password) < 1 {
			writeFailureResponse(w, r, http.StatusUnauthorized, "")
			return
		}
	}

	valid, err := checkUserPassword(username, password, state.Config)
	if err != nil {
		writeFailureResponse(w, r, http.StatusInternalServerError, "")
		return
	}
	if !valid {
		writeFailureResponse(w, r, http.StatusUnauthorized, "")
		log.Printf("Invalid login for %s", username)
		//err := errors.New("Invalid Credentials")
		return
	}
	//
	cookieVal, err := genRandomString()
	if err != nil {
		writeFailureResponse(w, r, http.StatusInternalServerError, "error internal")
		log.Println(err)
		return
	}

	expiration := time.Now().Add(time.Duration(maxAgeSecondsAuthCookie) * time.Second)
	savedUserInfo := authInfo{Username: username, ExpiresAt: expiration}
	state.Mutex.Lock()
	state.authCookie[cookieVal] = savedUserInfo
	state.Mutex.Unlock()

	authCookie := http.Cookie{Name: authCookieName, Value: cookieVal, Expires: expiration, Path: "/", HttpOnly: true, Secure: true}

	//use handler with original request.
	http.SetCookie(w, &authCookie)

	//return user, nil

	//log.Printf("cert type =%s", certType)
	returnAcceptType := "application/json"
	acceptHeader, ok := r.Header["Accept"]
	if ok {
		for _, acceptValue := range acceptHeader {
			if strings.Contains(acceptValue, "text/html") {
				log.Printf("Got it  %+v", acceptValue)
				returnAcceptType = "text/html"
			}
		}
	}
	switch returnAcceptType {
	case "text/html":
		http.Redirect(w, r, profilePath, 302)
	default:
		w.WriteHeader(200)
		fmt.Fprintf(w, "Success!")
	}
	return

}

////////////////////////////

func getRegistrationArray(U2fAuthData []u2fAuthData) (regArray []u2f.Registration) {
	for _, data := range U2fAuthData {
		regArray = append(regArray, *data.Registration)
	}
	return regArray
}

const u2fRegustisterRequestPath = "/u2f/RegisterRequest"

func (state *RuntimeState) u2fRegisterRequest(w http.ResponseWriter, r *http.Request) {
	// User must be logged in
	var signerIsNull bool

	// copy runtime singer if not nil
	state.Mutex.Lock()
	signerIsNull = (state.Signer == nil)
	state.Mutex.Unlock()

	//local sanity tests
	if signerIsNull {
		writeFailureResponse(w, r, http.StatusInternalServerError, "")
		log.Printf("Signer not loaded")
		return
	}
	/*
	 */
	// TODO(camilo_viecco1): reorder checks so that simple checks are done before checking user creds
	authUser, err := checkAuth(w, r, state)
	if err != nil {
		log.Printf("%v", err)

		return
	}

	// This is an UGLY big lock... we should at least create a separate lock for
	// the userProfile struct
	state.Mutex.Lock()
	defer state.Mutex.Unlock()
	profile, _ := state.userProfile[authUser]

	c, err := u2f.NewChallenge(u2fAppID, u2fTrustedFacets)
	if err != nil {
		log.Printf("u2f.NewChallenge error: %v", err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	profile.RegistrationChallenge = c
	registrations := getRegistrationArray(profile.U2fAuthData)
	req := u2f.NewWebRegisterRequest(c, registrations)

	log.Printf("registerRequest: %+v", req)
	state.userProfile[authUser] = profile
	json.NewEncoder(w).Encode(req)
}

const u2fRegisterRequesponsePath = "/u2f/RegisterResponse"

func (state *RuntimeState) u2fRegisterResponse(w http.ResponseWriter, r *http.Request) {
	// User must be logged in
	var signerIsNull bool

	// copy runtime singer if not nil
	state.Mutex.Lock()
	signerIsNull = (state.Signer == nil)
	state.Mutex.Unlock()

	//local sanity tests
	if signerIsNull {
		writeFailureResponse(w, r, http.StatusInternalServerError, "")
		log.Printf("Signer not loaded")
		return
	}
	/*
	 */
	// TODO(camilo_viecco1): reorder checks so that simple checks are done before checking user creds
	authUser, err := checkAuth(w, r, state)
	if err != nil {
		log.Printf("%v", err)

		return
	}

	var regResp u2f.RegisterResponse
	if err := json.NewDecoder(r.Body).Decode(&regResp); err != nil {
		http.Error(w, "invalid response: "+err.Error(), http.StatusBadRequest)
		return
	}

	state.Mutex.Lock()
	defer state.Mutex.Unlock()
	profile, _ := state.userProfile[authUser]

	if profile.RegistrationChallenge == nil {
		http.Error(w, "challenge not found", http.StatusBadRequest)
		return
	}

	// TODO: use yubikey or get the feitan cert :(
	u2fConfig := u2f.Config{SkipAttestationVerify: true}

	reg, err := u2f.Register(regResp, *profile.RegistrationChallenge, &u2fConfig)
	if err != nil {
		log.Printf("u2f.Register error: %v", err)
		http.Error(w, "error verifying response", http.StatusInternalServerError)
		return
	}

	newReg := u2fAuthData{Counter: 0, Registration: reg}
	profile.U2fAuthData = append(profile.U2fAuthData, newReg)
	//registrations = append(registrations, *reg)
	//counter = 0

	log.Printf("Registration success: %+v", reg)

	profile.RegistrationChallenge = nil
	state.userProfile[authUser] = profile

	// TODO: make goroutine!
	state.SaveUserProfiles()

	w.Write([]byte("success"))
}

const u2fSignRequestPath = "/u2f/SignRequest"

func (state *RuntimeState) u2fSignRequest(w http.ResponseWriter, r *http.Request) {
	/// Check if unlocked

	// User must be logged in
	var signerIsNull bool

	// copy runtime singer if not nil
	state.Mutex.Lock()
	signerIsNull = (state.Signer == nil)
	state.Mutex.Unlock()

	//local sanity tests
	if signerIsNull {
		writeFailureResponse(w, r, http.StatusInternalServerError, "")
		log.Printf("Signer not loaded")
		return
	}
	/*
	 */
	// TODO(camilo_viecco1): reorder checks so that simple checks are done before checking user creds
	authUser, err := checkAuth(w, r, state)
	if err != nil {
		log.Printf("%v", err)

		return
	}

	//////////
	state.Mutex.Lock()
	defer state.Mutex.Unlock()
	profile, ok := state.userProfile[authUser]

	/////////
	if !ok {
		http.Error(w, "No regstered data", http.StatusBadRequest)
		return
	}
	registrations := getRegistrationArray(profile.U2fAuthData)
	if len(registrations) < 1 {
		http.Error(w, "registration missing", http.StatusBadRequest)
		return
	}

	c, err := u2f.NewChallenge(u2fAppID, u2fTrustedFacets)
	if err != nil {
		log.Printf("u2f.NewChallenge error: %v", err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	profile.u2fAuthChallenge = c
	state.userProfile[authUser] = profile

	req := c.SignRequest(registrations)
	log.Printf("Sign request: %+v", req)

	json.NewEncoder(w).Encode(req)
}

const u2fSignResponsePath = "/u2f/SignResponse"

func (state *RuntimeState) u2fSignResponse(w http.ResponseWriter, r *http.Request) {
	// User must be logged in
	var signerIsNull bool

	// copy runtime singer if not nil
	state.Mutex.Lock()
	signerIsNull = (state.Signer == nil)
	state.Mutex.Unlock()

	//local sanity tests
	if signerIsNull {
		writeFailureResponse(w, r, http.StatusInternalServerError, "")
		log.Printf("Signer not loaded")
		return
	}
	/*
	 */
	// TODO(camilo_viecco1): reorder checks so that simple checks are done before checking user creds
	authUser, err := checkAuth(w, r, state)
	if err != nil {
		log.Printf("%v", err)

		return
	}
	//now the actual work
	var signResp u2f.SignResponse
	if err := json.NewDecoder(r.Body).Decode(&signResp); err != nil {
		http.Error(w, "invalid response: "+err.Error(), http.StatusBadRequest)
		return
	}

	log.Printf("signResponse: %+v", signResp)

	state.Mutex.Lock()
	defer state.Mutex.Unlock()
	profile, ok := state.userProfile[authUser]

	/////////
	if !ok {
		http.Error(w, "No regstered data", http.StatusBadRequest)
		return
	}
	registrations := getRegistrationArray(profile.U2fAuthData)
	if len(registrations) < 1 {
		http.Error(w, "registration missing", http.StatusBadRequest)
		return
	}

	if profile.u2fAuthChallenge == nil {
		http.Error(w, "challenge missing", http.StatusBadRequest)
		return
	}
	if registrations == nil {
		http.Error(w, "registration missing", http.StatusBadRequest)
		return
	}

	//var err error
	for i, u2fReg := range profile.U2fAuthData {
		newCounter, authErr := u2fReg.Registration.Authenticate(signResp, *profile.u2fAuthChallenge, u2fReg.Counter)
		if authErr == nil {
			log.Printf("newCounter: %d", newCounter)
			//counter = newCounter
			profile.U2fAuthData[i].Counter = newCounter
			profile.u2fAuthChallenge = nil
			state.userProfile[authUser] = profile

			// TODO: make goroutine!
			state.SaveUserProfiles()

			// TODO: update local cookie state
			w.Write([]byte("success"))
			return
		}
	}

	log.Printf("VerifySignResponse error: %v", err)
	http.Error(w, "error verifying response", http.StatusInternalServerError)
}

const profilePath = "/profile/"

const indexHTML = `<!DOCTYPE html>
<html>
  <head>
    <script src="//code.jquery.com/jquery-1.12.4.min.js"></script>
    <!-- The original u2f-api.js code can be found here:
    https://github.com/google/u2f-ref-code/blob/master/u2f-gae-demo/war/js/u2f-api.js -->
    <script type="text/javascript" src="/static/u2f-api.js"></script>
    <!-- script type="text/javascript" src="https://demo.yubico.com/js/u2f-api.js"></script-->
  </head>
  <body>
    <h1>FIDO U2F Go Library Demo</h1>
    <ul>
      <li><a href="javascript:register();">Register token</a></li>
      <li><a href="javascript:sign();">Authenticate</a></li>
    </ul>
    <p>Open Chrome Developer Tools to see debug console logs.</p>
    <script>
  function serverError(data) {
    console.log(data);
    alert('Server error code ' + data.status + ': ' + data.responseText);
  }
  function checkError(resp) {
    if (!('errorCode' in resp)) {
      return false;
    }
    if (resp.errorCode === u2f.ErrorCodes['OK']) {
      return false;
    }
    var msg = 'U2F error code ' + resp.errorCode;
    for (name in u2f.ErrorCodes) {
      if (u2f.ErrorCodes[name] === resp.errorCode) {
        msg += ' (' + name + ')';
      }
    }
    if (resp.errorMessage) {
      msg += ': ' + resp.errorMessage;
    }
    console.log(msg);
    alert(msg);
    return true;
  }
  function u2fRegistered(resp) {
    console.log(resp);
    if (checkError(resp)) {
      return;
    }
    $.post('/u2f/RegisterResponse', JSON.stringify(resp)).success(function() {
      alert('Success');
    }).fail(serverError);
  }
  function register() {
    $.getJSON('/u2f/RegisterRequest').success(function(req) {
      console.log(req);
      u2f.register(req.appId, req.registerRequests, req.registeredKeys, u2fRegistered, 30);
    }).fail(serverError);
  }
  function u2fSigned(resp) {
    console.log(resp);
    if (checkError(resp)) {
      return;
    }
    $.post('/u2f/SignResponse', JSON.stringify(resp)).success(function() {
      alert('Success');
    }).fail(serverError);
  }
  function sign() {
    $.getJSON('/u2f/SignRequest').success(function(req) {
      console.log(req);
      u2f.sign(req.appId, req.challenge, req.registeredKeys, u2fSigned, 30);
    }).fail(serverError);
  }
    </script>
  </body>
</html>
`

func (state *RuntimeState) profileHandler(w http.ResponseWriter, r *http.Request) {
	// User must be logged in
	var signerIsNull bool

	// copy runtime singer if not nil
	state.Mutex.Lock()
	signerIsNull = (state.Signer == nil)
	state.Mutex.Unlock()

	//local sanity tests
	if signerIsNull {
		writeFailureResponse(w, r, http.StatusInternalServerError, "")
		log.Printf("Signer not loaded")
		return
	}
	/*
	 */
	// TODO(camilo_viecco1): reorder checks so that simple checks are done before checking user creds
	_, err := checkAuth(w, r, state)
	if err != nil {
		log.Printf("%v", err)

		return
	}
	w.Write([]byte(indexHTML))
}

func Usage() {
	fmt.Fprintf(os.Stderr, "Usage of %s (version %s):\n", os.Args[0], Version)
	flag.PrintDefaults()
}

func main() {
	flag.Usage = Usage
	flag.Parse()

	circularBuffer := logbuf.New()
	if circularBuffer == nil {
		panic("Cannot create circular buffer")
	}
	log.New(circularBuffer, "", log.LstdFlags)

	runtimeState, err := loadVerifyConfigFile(*configFilename)
	if err != nil {
		panic(err)
	}
	if *debug || true {
		log.Printf("After load verify")
	}

	// Expose the registered metrics via HTTP.
	http.Handle("/metrics", prometheus.Handler())
	http.HandleFunc(SECRETINJECTOR_PATH, runtimeState.secretInjectorHandler)
	http.HandleFunc(CERTGEN_PATH, runtimeState.certGenHandler)
	http.HandleFunc(PUBLIC_PATH, runtimeState.publicPathHandler)
	http.HandleFunc(LOGIN_PATH, runtimeState.loginHandler)

	http.HandleFunc(profilePath, runtimeState.profileHandler)
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static_files"))))
	http.HandleFunc(u2fRegustisterRequestPath, runtimeState.u2fRegisterRequest)
	http.HandleFunc(u2fRegisterRequesponsePath, runtimeState.u2fRegisterResponse)
	http.HandleFunc(u2fSignRequestPath, runtimeState.u2fSignRequest)
	http.HandleFunc(u2fSignResponsePath, runtimeState.u2fSignResponse)

	cfg := &tls.Config{
		ClientCAs:                runtimeState.ClientCAPool,
		ClientAuth:               tls.VerifyClientCertIfGiven,
		MinVersion:               tls.VersionTLS12,
		CurvePreferences:         []tls.CurveID{tls.CurveP521, tls.CurveP384, tls.CurveP256},
		PreferServerCipherSuites: true,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
		},
	}
	srv := &http.Server{
		Addr:         runtimeState.Config.Base.HttpAddress,
		TLSConfig:    cfg,
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler), 0),
	}

	err = srv.ListenAndServeTLS(
		runtimeState.Config.Base.TLSCertFilename,
		runtimeState.Config.Base.TLSKeyFilename)
	if err != nil {
		panic(err)
	}
}
