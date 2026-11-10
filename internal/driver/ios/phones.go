package ios

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// The phones the person uses over Wi-Fi, kept in Provider.PhonesFile, so
// that a Pinky started while they are off the cable looks for them on the
// network (pairedUDIDs) and uses them there: on macOS the pair records are
// readable by usbmuxd only, so the UDIDs are known otherwise only once a
// phone was seen on a cable.

// readPhones returns the UDIDs in the file at path, or none.
func readPhones(path string) []string {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var ids []string
	if json.Unmarshal(b, &ids) != nil {
		return nil
	}
	return ids
}

// UseSavedPhones marks the phones in PhonesFile as used over Wi-Fi.
func (p *Provider) UseSavedPhones() {
	for _, id := range readPhones(p.PhonesFile) {
		p.UseOverWiFi(id)
	}
}

// savePhonesLocked adds the phones the person uses to PhonesFile. The
// caller holds p.mu.
func (p *Provider) savePhonesLocked() {
	if p.PhonesFile == "" {
		return
	}
	ids := map[string]bool{}
	for _, id := range readPhones(p.PhonesFile) {
		ids[id] = true
	}
	n := len(ids)
	for id := range p.wifi.use {
		ids[id] = true
	}
	if len(ids) == n {
		return
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	b, err := json.MarshalIndent(out, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(p.PhonesFile), 0o700)
	}
	if err == nil {
		err = os.WriteFile(p.PhonesFile, b, 0o600)
	}
	if err != nil {
		p.logger().Warn("iphone: could not keep the phones used over Wi-Fi", "err", err)
	}
}
