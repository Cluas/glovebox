package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// updateCmd is the release tooling, run on the build machine:
//
//	glovebox update keygen -out key            new signing key; prints the public key
//	glovebox update sign -key key -zip z.zip   prints update.json for the zip
func updateCmd(args []string) error {
	usage := errors.New("usage: glovebox update keygen -out file | update sign -key file -zip file [-url url] [-notes text] [-out file]")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "keygen":
		fs := flag.NewFlagSet("update keygen", flag.ExitOnError)
		out := fs.String("out", "", "file for the private key (created with mode 0600, never overwritten)")
		fs.Parse(args[1:])
		if *out == "" {
			return usage
		}
		return updateKeygen(*out, os.Stdout)
	case "sign":
		fs := flag.NewFlagSet("update sign", flag.ExitOnError)
		key := fs.String("key", "", "private key file from update keygen")
		zipPath := fs.String("zip", "", "module zip from build.sh")
		zipURL := fs.String("url", "", "zip location, absolute or relative to update.json (default: the zip's file name)")
		notes := fs.String("notes", "", "release notes shown on the device")
		out := fs.String("out", "", "write update.json here instead of stdout")
		fs.Parse(args[1:])
		if *key == "" || *zipPath == "" {
			return usage
		}
		if *zipURL == "" {
			*zipURL = filepath.Base(*zipPath)
		}
		m, pub, err := signUpdate(*key, *zipPath, *zipURL, *notes)
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(m, "", "  ")
		b = append(b, '\n')
		if !slices.Contains(trustedUpdateKeys, pub) {
			fmt.Fprintf(os.Stderr, "note: key %s is not built in; devices need it in update_pubkeys\n", pub)
		}
		if *out == "" {
			_, err = os.Stdout.Write(b)
			return err
		}
		return writeFileAtomic(*out, b, 0o644)
	}
	return usage
}

func updateKeygen(path string, w io.Writer) error {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, base64.StdEncoding.EncodeToString(pub))
	return err
}

func readSigningKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s: not a key from update keygen", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// signUpdate builds the signed manifest for a module zip; the version comes from its module.prop
func signUpdate(keyPath, zipPath, zipURL, notes string) (manifest, string, error) {
	priv, err := readSigningKey(keyPath)
	if err != nil {
		return manifest{}, "", err
	}
	b, err := os.ReadFile(zipPath)
	if err != nil {
		return manifest{}, "", err
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return manifest{}, "", fmt.Errorf("%s: %w", zipPath, err)
	}
	var props map[string]string
	for _, f := range zr.File {
		if strings.TrimPrefix(f.Name, "./") != "module.prop" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return manifest{}, "", err
		}
		pb, err := io.ReadAll(io.LimitReader(rc, 64<<10))
		rc.Close()
		if err != nil {
			return manifest{}, "", err
		}
		props = parseProps(pb)
	}
	if props == nil {
		return manifest{}, "", fmt.Errorf("%s: no module.prop", zipPath)
	}
	code, err := strconv.ParseInt(props["versionCode"], 10, 64)
	if err != nil {
		return manifest{}, "", fmt.Errorf("module.prop: versionCode %q", props["versionCode"])
	}
	sum := sha256.Sum256(b)
	m := manifest{Version: props["version"], VersionCode: code, ZipURL: zipURL, SHA256: hex.EncodeToString(sum[:]),
		Size: int64(len(b)), Notes: notes}
	m.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m.signedText()))
	pub := priv.Public().(ed25519.PublicKey)
	if err := m.verify([]ed25519.PublicKey{pub}); err != nil {
		return manifest{}, "", err
	}
	return m, base64.StdEncoding.EncodeToString(pub), nil
}
