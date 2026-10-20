package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"time"
)

// diagCmd signs a diagnostics job, run on the build machine:
//
//	glovebox diag sign -key key -device name -in probes.json   prints diag.json
func diagCmd(args []string) error {
	usage := errors.New("usage: glovebox diag sign -key file -device name [-in file] [-id id] [-ttl 1h] [-out file]")
	if len(args) == 0 || args[0] != "sign" {
		return usage
	}
	fs := flag.NewFlagSet("diag sign", flag.ExitOnError)
	key := fs.String("key", "", "private key file from update keygen")
	device := fs.String("device", "", "device_name of the device that runs the job")
	in := fs.String("in", "-", `probes: a JSON array, or an object with "probes"; - reads stdin`)
	id := fs.String("id", "", "job id, run once per device (default: the current time)")
	ttl := fs.Duration("ttl", time.Hour, "how long the job stays valid, at most 24h")
	out := fs.String("out", "", "write diag.json here instead of stdout")
	fs.Parse(args[1:])
	if *key == "" || *device == "" {
		return usage
	}
	var b []byte
	var err error
	if *in == "-" {
		b, err = io.ReadAll(io.LimitReader(os.Stdin, diagJobMax))
	} else {
		b, err = os.ReadFile(*in)
	}
	if err != nil {
		return err
	}
	probes, err := parseProbes(b)
	if err != nil {
		return fmt.Errorf("%s: %w", *in, err)
	}
	priv, err := readSigningKey(*key)
	if err != nil {
		return err
	}
	if *id == "" {
		*id = time.Now().Format("20060102-150405")
	}
	j, err := signDiag(priv, *id, *device, *ttl, probes, time.Now())
	if err != nil {
		return err
	}
	pub := base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	if !slices.Contains(trustedUpdateKeys, pub) {
		fmt.Fprintf(os.Stderr, "note: key %s is not built in; devices need it in update_pubkeys\n", pub)
	}
	ob, _ := json.MarshalIndent(j, "", "  ")
	ob = append(ob, '\n')
	if *out == "" {
		_, err = os.Stdout.Write(ob)
		return err
	}
	return writeFileAtomic(*out, ob, 0o644)
}

// parseProbes is strict, unlike the device: a misspelled field would otherwise be dropped unnoticed
func parseProbes(b []byte) ([]diagProbe, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var probes []diagProbe
	if t := bytes.TrimSpace(b); len(t) > 0 && t[0] == '[' {
		err := dec.Decode(&probes)
		return probes, err
	}
	var j struct {
		Probes []diagProbe `json:"probes"`
	}
	err := dec.Decode(&j)
	return j.Probes, err
}

// signDiag builds and signs a job, checked the way the device will check it
func signDiag(priv ed25519.PrivateKey, id, device string, ttl time.Duration, probes []diagProbe, now time.Time) (diagJob, error) {
	j := diagJob{ID: id, Device: device, NotAfter: now.Add(ttl).Format(time.RFC3339), Probes: probes}
	j.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, j.signedText()))
	if err := j.verify([]ed25519.PublicKey{priv.Public().(ed25519.PublicKey)}, device, now); err != nil {
		return diagJob{}, err
	}
	return j, nil
}
