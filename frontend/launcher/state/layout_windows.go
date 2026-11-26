package state

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func DefaultRoot() string {
	dir := os.Getenv("ProgramData")
	if dir == "" {
		dir = filepath.Join(os.Getenv("SystemDrive")+string(os.PathSeparator), "ProgramData")
	}
	return filepath.Join(dir, "GhostHand")
}

// Initialize is called by the elevated installer before service or enrollment startup.
func (l Layout) Initialize() error {
	root := filepath.Clean(l.Root)
	if !filepath.IsAbs(root) {
		return errors.New("Windows data directory must be absolute")
	}
	const private = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	if err := secureDirectory(root, private+"(A;;0x1200a9;;;BU)"); err != nil {
		return err
	}
	for _, name := range []string{"runtime", "versions"} {
		dir := filepath.Join(root, name)
		acl := private
		if name == "versions" {
			acl += "(A;OICI;0x1200a9;;;BU)"
		}
		if err := secureDirectory(dir, acl); err != nil {
			return err
		}
	}
	return nil
}

func PrepareBrowserExtensionDirectory(root string) (string, error) {
	const private = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	if err := secureDirectory(root, private+"(A;;0x1200a9;;;BU)"); err != nil {
		return "", err
	}
	dir := filepath.Join(root, "chrome-extension")
	return dir, secureDirectory(dir, private+"(A;OICI;0x1200a9;;;BU)")
}

func secureDirectory(path, sddl string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("O:BA" + sddl)
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(p, &attributes); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	attr, err := windows.GetFileAttributes(p)
	if err != nil {
		return err
	}
	if attr&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || attr&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return errors.New("GhostHand data directory must be a regular directory")
	}
	current, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := current.Owner()
	if err != nil {
		return err
	}
	if !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) && !owner.IsWellKnown(windows.WinLocalSystemSid) {
		return errors.New("GhostHand data directory must be owned by SYSTEM or Administrators")
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
