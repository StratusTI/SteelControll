package scripts

import (
	"os"
	"os/user"
	"strings"
	"sync"
)

var (
	cachedUsername     string
	cachedUsernameOnce sync.Once
)

// GetFullUsername retorna o FullName do usuário local do Windows via
// PowerShell: `(Get-LocalUser -Name $env:USERNAME).FullName`.
// O valor é cacheado por toda a vida do processo (não muda em runtime).
// Caso o PowerShell falhe ou o FullName esteja vazio, cai para user.Current().Name
// e por fim para os.Getenv("USERNAME").
func GetFullUsername() string {
	cachedUsernameOnce.Do(func() {
		cmd := hiddenCmd("powershell", "-NoProfile", "-NonInteractive", "-Command",
			"(Get-LocalUser -Name $env:USERNAME).FullName")
		if out, err := cmd.Output(); err == nil {
			if name := strings.TrimSpace(string(out)); name != "" {
				cachedUsername = name
				return
			}
		}
		if u, err := user.Current(); err == nil && u.Name != "" {
			cachedUsername = u.Name
			return
		}
		cachedUsername = os.Getenv("USERNAME")
	})
	return cachedUsername
}
