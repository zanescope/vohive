//go:build !unix

package modemmanager

func syncDirectory(string) error {
	return nil
}
