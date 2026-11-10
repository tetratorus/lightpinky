//go:build !darwin

package mdns

func refusedByPrivacy(error) bool { return false }
