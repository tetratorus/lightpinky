package apppkg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"strings"
)

// An APK's signer: the certificate of the APK Signature Scheme v3 or v2
// block (source.android.com/docs/security/features/apksigning), or of the
// JAR signature (v1) for APKs signed only that way. Pinky does not verify
// the signature; the phone does when it installs. The certificate only
// names the signer for the person, and tells one signer from another.

// Signing block ids.
const (
	blockV2 = 0x7109871a
	blockV3 = 0xf05368c0
)

var errNoSigningBlock = errors.New("no APK signing block")

// Cert is a signing certificate.
type Cert struct {
	Name   string // the subject's common name, else the whole subject
	SHA256 string // of the certificate, in lowercase hex
}

func certOf(der []byte) (Cert, error) {
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return Cert{}, err
	}
	sum := sha256.Sum256(der)
	name := c.Subject.CommonName
	if name == "" {
		name = c.Subject.String()
	}
	return Cert{Name: name, SHA256: hex.EncodeToString(sum[:])}, nil
}

// blockSigner reads the first signer's first certificate from the APK
// signing block of the file r, size bytes long.
func blockSigner(r io.ReaderAt, size int64) (Cert, error) {
	block, err := signingBlock(r, size)
	if err != nil {
		return Cert{}, err
	}
	pairs := map[uint32][]byte{}
	for b := block; len(b) >= 12; {
		n := binary.LittleEndian.Uint64(b)
		if n < 4 || n > uint64(len(b)-8) {
			break
		}
		pairs[binary.LittleEndian.Uint32(b[8:])] = b[12 : 8+n]
		b = b[8+n:]
	}
	for _, id := range []uint32{blockV3, blockV2} {
		if v, ok := pairs[id]; ok {
			der, err := schemeCert(v)
			if err != nil {
				return Cert{}, err
			}
			return certOf(der)
		}
	}
	return Cert{}, errNoSigningBlock
}

// signingBlock returns the pairs of the APK signing block, which sits
// right before the ZIP central directory.
func signingBlock(r io.ReaderAt, size int64) ([]byte, error) {
	tail := int64(65535 + 22)
	if tail > size {
		tail = size
	}
	buf := make([]byte, tail)
	if _, err := r.ReadAt(buf, size-tail); err != nil && err != io.EOF {
		return nil, err
	}
	eocd := -1
	for i := len(buf) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(buf[i:]) == 0x06054b50 {
			eocd = i
			break
		}
	}
	if eocd < 0 {
		return nil, errors.New("not a ZIP file")
	}
	cd := int64(binary.LittleEndian.Uint32(buf[eocd+16:]))
	if cd < 32 || cd > size {
		return nil, errNoSigningBlock
	}
	var foot [24]byte
	if _, err := r.ReadAt(foot[:], cd-24); err != nil {
		return nil, err
	}
	if string(foot[8:]) != "APK Sig Block 42" {
		return nil, errNoSigningBlock
	}
	n := int64(binary.LittleEndian.Uint64(foot[:]))
	if n < 24 || n > cd-8 || n > 64<<20 {
		return nil, errNoSigningBlock
	}
	block := make([]byte, n-24)
	if _, err := r.ReadAt(block, cd-n); err != nil {
		return nil, err
	}
	return block, nil
}

// lp reads a length-prefixed (uint32) value.
func lp(b []byte) (v, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, errShort
	}
	n := binary.LittleEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return nil, nil, errShort
	}
	return b[4 : 4+n], b[4+n:], nil
}

// schemeCert reads the first certificate of a v2 or v3 block's first
// signer: signers { signer { signed data { digests, certificates, … } … } }.
func schemeCert(v []byte) ([]byte, error) {
	signers, _, err := lp(v)
	if err != nil {
		return nil, err
	}
	signer, _, err := lp(signers)
	if err != nil {
		return nil, err
	}
	signed, _, err := lp(signer)
	if err != nil {
		return nil, err
	}
	_, rest, err := lp(signed) // digests
	if err != nil {
		return nil, err
	}
	certs, _, err := lp(rest)
	if err != nil {
		return nil, err
	}
	cert, _, err := lp(certs)
	if err != nil {
		return nil, err
	}
	return cert, nil
}

// jarSigner reads the certificate of a v1 (JAR) signature: the PKCS#7
// SignedData in META-INF/*.RSA, *.DSA or *.EC.
func jarSigner(z *zip.Reader) (Cert, error) {
	for _, f := range z.File {
		dir, name := path.Split(f.Name)
		ext := strings.ToUpper(path.Ext(name))
		if dir != "META-INF/" || (ext != ".RSA" && ext != ".DSA" && ext != ".EC") {
			continue
		}
		b, err := readEntry(f, 1<<20)
		if err != nil {
			return Cert{}, err
		}
		der, err := pkcs7Cert(b)
		if err != nil {
			return Cert{}, err
		}
		return certOf(der)
	}
	return Cert{}, errors.New("the APK is not signed")
}

func pkcs7Cert(b []byte) ([]byte, error) {
	var ci struct {
		Type    asn1.ObjectIdentifier
		Content asn1.RawValue `asn1:"explicit,tag:0"`
	}
	if _, err := asn1.Unmarshal(b, &ci); err != nil {
		return nil, err
	}
	var sd struct {
		Version          int
		DigestAlgorithms asn1.RawValue
		Content          asn1.RawValue
		Certificates     asn1.RawValue `asn1:"optional,tag:0"`
		CRLs             asn1.RawValue `asn1:"optional,tag:1"`
		SignerInfos      asn1.RawValue
	}
	if _, err := asn1.Unmarshal(ci.Content.Bytes, &sd); err != nil {
		return nil, err
	}
	var cert asn1.RawValue
	if _, err := asn1.Unmarshal(sd.Certificates.Bytes, &cert); err != nil {
		return nil, errors.New("the signature holds no certificate")
	}
	return cert.FullBytes, nil
}

// readEntry reads a ZIP entry of at most max bytes.
func readEntry(f *zip.File, max int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(max) {
		return nil, errors.New(f.Name + " is too large")
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var b bytes.Buffer
	if _, err := io.Copy(&b, io.LimitReader(rc, max+1)); err != nil {
		return nil, err
	}
	if int64(b.Len()) > max {
		return nil, errors.New(f.Name + " is too large")
	}
	return b.Bytes(), nil
}

// Text names the certificate for people: its name and the start of its
// SHA-256, "Pinky Test (292579aa)".
func (c Cert) Text() string {
	short := c.SHA256
	if len(short) > 8 {
		short = short[:8]
	}
	if c.Name == "" {
		return short
	}
	return c.Name + " (" + short + ")"
}
