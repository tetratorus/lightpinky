package driver

import "os"

// PairRecordDir is where Apple's device service keeps this computer's
// pairing records for iPhones on Windows. Apple Mobile Device Service writes
// them while an iPhone is paired, and every later session with the phone
// reads them from there. ProgramData can be on another drive than C:.
func PairRecordDir() string {
	if dir := os.Getenv("ProgramData"); dir != "" {
		return dir + `\Apple\Lockdown`
	}
	return `C:\ProgramData\Apple\Lockdown`
}

// PairRecordFile is the pairing record this computer holds for one iPhone.
func PairRecordFile(udid string) string {
	return PairRecordDir() + `\` + udid + ".plist"
}

// PairRecordFixCommand is the command that lets Apple's device service
// replace an iPhone's pairing record, run as an administrator in PowerShell:
// it gives the service (LOCAL SERVICE, S-1-5-19) the right to modify the
// file, which is all it needs to replace its contents. The service runs as
// LOCAL SERVICE, and a record that another install of Apple's software wrote,
// as SYSTEM, is one it may read but not replace, so the pairing it saves
// never lands and the phone refuses every session with the record it still
// holds. Modify rather than full control: the record holds this computer's
// private key for the phone, and full control would also let any LOCAL
// SERVICE process change the file's permissions and owner.
func PairRecordFixCommand(udid string) string {
	return `icacls "$env:ProgramData\Apple\Lockdown\` + udid + `.plist" /grant "*S-1-5-19:(M)"`
}
