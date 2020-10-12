package certinject

import (
	"crypto/sha1" // #nosec G505
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
	"gopkg.in/hlandau/easyconfig.v1/cflag"

	"github.com/namecoin/certinject/certblob"
)

var (
	cryptoAPIFlagGroup            = cflag.NewGroup(flagGroup, "capi")
	cryptoAPIFlagLogicalStoreName = cflag.String(cryptoAPIFlagGroup, "logical-store", "Root",
		"Name of CryptoAPI logical store to inject certificate into. Consider: AuthRoot, Root, Trust, CA, My, Disallowed")
	cryptoAPIFlagPhysicalStoreName = cflag.String(cryptoAPIFlagGroup, "physical-store", "system",
		"Scope of CryptoAPI certificate store. Valid choices: current-user, system, enterprise, group-policy")
	cryptoAPIFlagReset = cflag.Bool(cryptoAPIFlagGroup, "reset", false,
		"Delete any existing properties of this certificate before applying any new ones")
	ekuFlagGroup = cflag.NewGroup(cryptoAPIFlagGroup, "eku")
	ekuAny       = cflag.Bool(ekuFlagGroup, "any", false, "Any purpose")
	ekuServer    = cflag.Bool(ekuFlagGroup, "server", false,
		"Server authentication")
	ekuClient = cflag.Bool(ekuFlagGroup, "client", false,
		"Client authentication")
	ekuCode  = cflag.Bool(ekuFlagGroup, "code", false, "Code signing")
	ekuEmail = cflag.Bool(ekuFlagGroup, "email", false,
		"Secure email")
	ekuIPSECEndSystem = cflag.Bool(ekuFlagGroup, "ipsec-end-system", false,
		"IP security end system")
	ekuIPSECTunnel = cflag.Bool(ekuFlagGroup, "ipsec-tunnel", false,
		"IP security tunnel termination")
	ekuIPSECUser = cflag.Bool(ekuFlagGroup, "ipsec-user", false,
		"IP security user")
	ekuTime = cflag.Bool(ekuFlagGroup, "time", false, "Time stamping")
	ekuOCSP = cflag.Bool(ekuFlagGroup, "ocsp", false, "OCSP signing")
	// We intentionally do not support "server-gated crypto" / "international
	// step-up" EKU values, because 90's-era export-grade crypto can go shove
	// its reproductive organs in a beehive.
	ekuMSCodeCom = cflag.Bool(ekuFlagGroup, "ms-code-com", false,
		"Microsoft commercial code signing")
	ekuMSCodeKernel = cflag.Bool(ekuFlagGroup, "ms-code-kernel", false,
		"Microsoft kernel-mode code signing")
	nameConstraintsFlagGroup    = cflag.NewGroup(cryptoAPIFlagGroup, "nc")
	nameConstraintsPermittedDNS = cflag.String(nameConstraintsFlagGroup,
		"permitted-dns", "", "Permitted DNS domain")
	nameConstraintsExcludedDNS = cflag.String(nameConstraintsFlagGroup,
		"excluded-dns", "", "Excluded DNS domain")
	nameConstraintsPermittedIP = cflag.String(nameConstraintsFlagGroup,
		"permitted-ip", "", "Permitted IP range")
	nameConstraintsExcludedIP = cflag.String(nameConstraintsFlagGroup,
		"excluded-ip", "", "Excluded IP range")
	nameConstraintsPermittedEmail = cflag.String(nameConstraintsFlagGroup,
		"permitted-email", "", "Permitted email address")
	nameConstraintsExcludedEmail = cflag.String(nameConstraintsFlagGroup,
		"excluded-email", "", "Excluded email address")
	nameConstraintsPermittedURI = cflag.String(nameConstraintsFlagGroup,
		"permitted-uri", "", "Permitted URI domain")
	nameConstraintsExcludedURI = cflag.String(nameConstraintsFlagGroup,
		"excluded-uri", "", "Excluded URI domain")
)

const cryptoAPIMagicName = "Namecoin"
const cryptoAPIMagicValue = 1

var ErrGetInitialBlob = errors.New("error getting initial blob")

var (
	// cryptoAPIStores consists of every implemented store.
	// when adding a new one, the `%s` variable is optional.
	// if `%s` exists in the Logical string, it is replaced with the value of -store flag
	cryptoAPIStores = map[string]Store{
		"current-user": Store{registry.CURRENT_USER, `SOFTWARE\Microsoft\SystemCertificates`, `%s\Certificates`},
		"system":       Store{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\SystemCertificates`, `%s\Certificates`},
		"enterprise":   Store{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\EnterpriseCertificates`, `%s\Certificates`},
		"group-policy": Store{registry.LOCAL_MACHINE, `SOFTWARE\Policies\Microsoft\SystemCertificates`, `%s\Certificates`},
	}
)

// Store is used to generate a registry key to open a certificate store in the Windows Registry.
type Store struct {
	Base     registry.Key
	Physical string
	Logical  string // may contain a %s, in which it would be replaced by the -store flag
}

// String returns a human readable string (only useful for debug logs).
func (s Store) String() string {
	return fmt.Sprintf(`%s\%s\`+s.Logical, s.Base, s.Physical, cryptoAPIFlagLogicalStoreName.Value())
}

// Key generates the registry key for use in opening the store.
func (s Store) Key() string {
	return fmt.Sprintf(`%s\`+s.Logical, s.Physical, cryptoAPIFlagLogicalStoreName.Value())
}

// cryptoAPINameToStore checks that the choice is valid before returning a complete Store request
func cryptoAPINameToStore(name string) (Store, error) {
	store, ok := cryptoAPIStores[name]
	if !ok {
		return Store{}, fmt.Errorf("invalid choice for physical store, consider: current-user, system, enterprise, group-policy")
	}

	return store, nil
}

func readInputBlob(derBytes []byte, registryBase registry.Key, path string) (certblob.Blob, error) {
	if cryptoAPIFlagReset.Value() && derBytes != nil {
		// We already know the cert preimage, and we're excluding any
		// properties, so no need to check the registry.
		return certblob.Blob{certblob.CertContentCertPropID: derBytes}, nil
	}

	// We need to look up either the cert preimage or the properties via
	// the registry.

	// Open up the cert key.
	certKey, err := registry.OpenKey(registryBase, path, registry.QUERY_VALUE)
	if err != nil && derBytes != nil {
		// We can't read the blob, but we do already know the cert
		// preimage, so create a default blob based on that preimage.
		return certblob.Blob{certblob.CertContentCertPropID: derBytes}, nil
	}
	defer certKey.Close()

	inputBlobBytes, _, err := certKey.GetBinaryValue("Blob")
	if err != nil {
		return nil, fmt.Errorf("%s: couldn't read blob value: %w", err, ErrGetInitialBlob)
	}

	blob, err := certblob.ParseBlob(inputBlobBytes)
	if err != nil {
		return nil, fmt.Errorf("%s: couldn't parse blob: %w", err, ErrGetInitialBlob)
	}

	return blob, nil
}

func injectCertCryptoAPI(derBytes []byte) {
	store, err := cryptoAPINameToStore(cryptoAPIFlagPhysicalStoreName.Value())
	if err != nil {
		log.Errorf("error: %s", err.Error())
		return
	}

	registryBase := store.Base
	storeKey := store.Key()

	// Windows CryptoAPI uses the SHA-1 fingerprint to identify a cert.
	// This is probably a Bad Thing (TM) since SHA-1 is weak.
	// However, that's Microsoft's problem to fix, not ours.
	fingerprint := sha1.Sum(derBytes) // #nosec G401

	// Windows CryptoAPI uses a hex string to represent the fingerprint.
	fingerprintHex := hex.EncodeToString(fingerprint[:])

	// Windows CryptoAPI uses uppercase hex strings
	fingerprintHexUpper := strings.ToUpper(fingerprintHex)

	// Format documentation of Microsoft's "Certificate Registry Blob":

	// 5c 00 00 00 // propid
	// 01 00 00 00 // unknown (possibly a version or flags field; value is always the same in my testing)
	// 04 00 00 00 // size (little endian)
	// subject public key bit length // data[size]

	// 19 00 00 00
	// 01 00 00 00
	// 10 00 00 00
	// MD5 of ECC pubkey of certificate

	// 0f 00 00 00
	// 01 00 00 00
	// 20 00 00 00
	// Signature Hash

	// 03 00 00 00
	// 01 00 00 00
	// 14 00 00 00
	// Cert SHA1 hash

	// 14 00 00 00
	// 01 00 00 00
	// 14 00 00 00
	// Key Identifier

	// 04 00 00 00
	// 01 00 00 00
	// 10 00 00 00
	// Cert MD5 hash

	// 20 00 00 00
	// 01 00 00 00
	// cert length
	// cert

	// But, guess what?  All you need is the "20" record.
	// Windows will happily regenerate all the others for you, whenever you actually try to use the certificate.
	// How cool is that?

	// Construct the input Blob
	blob, err := readInputBlob(derBytes, registryBase, storeKey+`\`+fingerprintHexUpper)
	if err != nil {
		log.Errorf("Couldn't read input blob: %s", err)
		return
	}

	ekus := []x509.ExtKeyUsage{}

	if ekuAny.Value() {
		ekus = append(ekus, x509.ExtKeyUsageAny)
	}

	if ekuServer.Value() {
		ekus = append(ekus, x509.ExtKeyUsageServerAuth)
	}

	if ekuClient.Value() {
		ekus = append(ekus, x509.ExtKeyUsageClientAuth)
	}

	if ekuCode.Value() {
		ekus = append(ekus, x509.ExtKeyUsageCodeSigning)
	}

	if ekuEmail.Value() {
		ekus = append(ekus, x509.ExtKeyUsageEmailProtection)
	}

	if ekuIPSECEndSystem.Value() {
		ekus = append(ekus, x509.ExtKeyUsageIPSECEndSystem)
	}

	if ekuIPSECTunnel.Value() {
		ekus = append(ekus, x509.ExtKeyUsageIPSECTunnel)
	}

	if ekuIPSECUser.Value() {
		ekus = append(ekus, x509.ExtKeyUsageIPSECUser)
	}

	if ekuTime.Value() {
		ekus = append(ekus, x509.ExtKeyUsageTimeStamping)
	}

	if ekuOCSP.Value() {
		ekus = append(ekus, x509.ExtKeyUsageOCSPSigning)
	}

	if ekuMSCodeCom.Value() {
		ekus = append(ekus, x509.ExtKeyUsageMicrosoftCommercialCodeSigning)
	}

	if ekuMSCodeKernel.Value() {
		ekus = append(ekus, x509.ExtKeyUsageMicrosoftKernelCodeSigning)
	}

	if len(ekus) > 0 {
		ekuTemplate := x509.Certificate{
			ExtKeyUsage: ekus,
		}

		ekuProperty, err := certblob.BuildExtKeyUsage(&ekuTemplate)
		if err != nil {
			log.Errorf("Couldn't marshal extended key usage property: %s", err)
			return
		}

		blob.SetProperty(ekuProperty)
	}

	nameConstraintsValid := false
	nameConstraintsTemplate := x509.Certificate{}

	if nameConstraintsPermittedDNS.Value() != "" {
		nameConstraintsTemplate.PermittedDNSDomains = []string{nameConstraintsPermittedDNS.Value()}
		nameConstraintsValid = true
	}

	if nameConstraintsExcludedDNS.Value() != "" {
		nameConstraintsTemplate.ExcludedDNSDomains = []string{nameConstraintsExcludedDNS.Value()}
		nameConstraintsValid = true
	}

	if nameConstraintsPermittedIP.Value() != "" {
		_, nameConstraintsPermittedIPNet, err := net.ParseCIDR(nameConstraintsPermittedIP.Value())
		if err != nil {
			log.Errorf("Couldn't parse permitted IP CIDR: %s", err)
			return
		}

		nameConstraintsTemplate.PermittedIPRanges = []*net.IPNet{nameConstraintsPermittedIPNet}
		nameConstraintsValid = true
	}

	if nameConstraintsExcludedIP.Value() != "" {
		_, nameConstraintsExcludedIPNet, err := net.ParseCIDR(nameConstraintsExcludedIP.Value())
		if err != nil {
			log.Errorf("Couldn't parse excluded IP CIDR: %s", err)
			return
		}

		nameConstraintsTemplate.ExcludedIPRanges = []*net.IPNet{nameConstraintsExcludedIPNet}
		nameConstraintsValid = true
	}

	if nameConstraintsPermittedEmail.Value() != "" {
		nameConstraintsTemplate.PermittedEmailAddresses = []string{nameConstraintsPermittedEmail.Value()}
		nameConstraintsValid = true
	}

	if nameConstraintsExcludedEmail.Value() != "" {
		nameConstraintsTemplate.ExcludedEmailAddresses = []string{nameConstraintsExcludedEmail.Value()}
		nameConstraintsValid = true
	}

	if nameConstraintsPermittedURI.Value() != "" {
		nameConstraintsTemplate.PermittedURIDomains = []string{nameConstraintsPermittedURI.Value()}
		nameConstraintsValid = true
	}

	if nameConstraintsExcludedURI.Value() != "" {
		nameConstraintsTemplate.ExcludedURIDomains = []string{nameConstraintsExcludedURI.Value()}
		nameConstraintsValid = true
	}

	if nameConstraintsValid {
		nameConstraintsProperty, err := certblob.BuildNameConstraints(&nameConstraintsTemplate)
		if err != nil {
			log.Errorf("Couldn't marshal name constraints property: %s", err)
			return
		}

		blob.SetProperty(nameConstraintsProperty)
	}

	// Marshal the Blob
	blobBytes, err := blob.Marshal()
	if err != nil {
		log.Errorf("Couldn't marshal cert blob: %s", err)
		return
	}

	// Open up the cert store.
	certStoreKey, err := registry.OpenKey(registryBase, storeKey, registry.ALL_ACCESS)
	if err != nil {
		log.Errorf("Couldn't open cert store: %s", err)
		return
	}
	defer certStoreKey.Close()

	// Create the registry key in which we will store the cert.
	// The 2nd result of CreateKey is openedExisting, which tells us if the cert already existed.
	// This doesn't matter to us.  If true, the "last modified" metadata won't update,
	// but we delete and recreate the magic value inside it as a workaround.
	certKey, _, err := registry.CreateKey(certStoreKey, fingerprintHexUpper, registry.ALL_ACCESS)
	if err != nil {
		log.Errorf("Couldn't create registry key for certificate: %s", err)
		return
	}
	defer certKey.Close()

	// Add a magic value which indicates that the certificate is a
	// Namecoin cert.  This will be used for deleting expired certs.
	// However, we have to delete it before we create it,
	// so that we make sure that the "last modified" metadata gets updated.
	// If an error occurs during deletion, we ignore it,
	// since it probably just means it wasn't there already.
	_ = certKey.DeleteValue(cryptoAPIMagicName)

	err = certKey.SetDWordValue(cryptoAPIMagicName, cryptoAPIMagicValue)
	if err != nil {
		log.Errorf("Couldn't set magic registry value for certificate: %s", err)
		return
	}

	// Create the registry value which holds the certificate.
	err = certKey.SetBinaryValue("Blob", blobBytes)
	if err != nil {
		log.Errorf("Couldn't set blob registry value for certificate: %s", err)
		return
	}
}

func cleanCertsCryptoAPI() {
	store, err := cryptoAPINameToStore(cryptoAPIFlagPhysicalStoreName.Value())
	if err != nil {
		log.Errorf("error: %s", err.Error())
		return
	}

	registryBase := store.Base
	storeKey := store.Key()

	// Open up the cert store.
	certStoreKey, err := registry.OpenKey(registryBase, storeKey, registry.ALL_ACCESS)
	if err != nil {
		log.Errorf("Couldn't open cert store: %s", err)
		return
	}
	defer certStoreKey.Close()

	// get all subkey names in the cert store
	subKeys, err := certStoreKey.ReadSubKeyNames(0)
	if err != nil {
		log.Errorf("Couldn't list certs in cert store: %s", err)
		return
	}

	// for all certs in the cert store
	for _, subKeyName := range subKeys {
		// Check if the cert is expired
		expired, err := checkCertExpiredCryptoAPI(certStoreKey, subKeyName)
		if err != nil {
			log.Errorf("Couldn't check if cert is expired: %s", err)
			return
		}

		// delete the cert if it's expired
		if expired {
			if err := registry.DeleteKey(certStoreKey, subKeyName); err != nil {
				log.Errorf("Coudn't delete expired cert: %s", err)
			}
		}
	}
}

func checkCertExpiredCryptoAPI(certStoreKey registry.Key, subKeyName string) (bool, error) {
	// Open the cert
	certKey, err := registry.OpenKey(certStoreKey, subKeyName, registry.ALL_ACCESS)
	if err != nil {
		return false, fmt.Errorf("Couldn't open cert registry key: %s", err)
	}
	defer certKey.Close()

	// Check for magic value
	isNamecoin, _, err := certKey.GetIntegerValue(cryptoAPIMagicName)
	if err != nil {
		// Magic value wasn't found.  Therefore don't consider it expired.
		return false, nil
	}

	if isNamecoin != cryptoAPIMagicValue {
		// Magic value was found but it wasn't the one we recognize.  Therefore don't consider it expired.
		return false, nil
	}

	// Get metadata about the cert key
	certKeyInfo, err := certKey.Stat()
	if err != nil {
		return false, fmt.Errorf("Couldn't read metadata for cert registry key: %s", err)
	}

	// Get the last modified time
	certKeyModTime := certKeyInfo.ModTime()

	// If the cert's last modified timestamp differs too much from the
	// current time in either direction, consider it expired
	expired := math.Abs(time.Since(certKeyModTime).Seconds()) > float64(certExpirePeriod.Value())

	return expired, nil
}
