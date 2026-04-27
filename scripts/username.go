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

// psGetUserIdentity espelha a função Get-UserIdentity validada em runtime:
//
//	Conta local  -> DOMINIO\FullName  (ex.: BASE06\9206)
//	Conta de AD  -> DOMINIO\SAM       (ex.: JUPITER\alexandre.stratus)
//
// Em AD o Get-LocalUser falha (UserNotFoundException) e caímos no SAM do
// ambiente ($env:USERDOMAIN\$env:USERNAME), que já é o nome "nome.sobrenome".
const psGetUserIdentity = `
$user   = $env:USERNAME
$domain = $env:USERDOMAIN
try {
    $full = (Get-LocalUser -Name $user -ErrorAction Stop).FullName
    if ($full) { "$domain\$full"; exit }
} catch {}
"$domain\$user"
`

// GetFullUsername retorna o identificador do usuário logado no formato
// DOMINIO\nome. Local usa o FullName da conta; AD usa o SAM name.
// O valor é cacheado por toda a vida do processo (não muda em runtime).
func GetFullUsername() string {
	cachedUsernameOnce.Do(func() {
		cmd := hiddenCmd("powershell", "-NoProfile", "-NonInteractive", "-Command", psGetUserIdentity)
		if out, err := cmd.Output(); err == nil {
			if name := strings.TrimSpace(string(out)); name != "" && name != `\` {
				cachedUsername = name
				return
			}
		}

		domain := strings.TrimSpace(os.Getenv("USERDOMAIN"))
		if u, err := user.Current(); err == nil {
			if u.Username != "" && strings.Contains(u.Username, `\`) {
				cachedUsername = u.Username
				return
			}
			if u.Name != "" {
				cachedUsername = joinDomainUser(domain, u.Name)
				return
			}
		}
		cachedUsername = joinDomainUser(domain, os.Getenv("USERNAME"))
	})
	return cachedUsername
}

func joinDomainUser(domain, user string) string {
	user = strings.TrimSpace(user)
	domain = strings.TrimSpace(domain)
	if user == "" {
		return ""
	}
	if domain == "" {
		return user
	}
	return domain + `\` + user
}
