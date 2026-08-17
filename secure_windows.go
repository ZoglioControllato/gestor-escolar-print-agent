//go:build windows

package main

import (
	"fmt"
	"strings"
)

// restrictFilePermissions aplica ACL restrita (SYSTEM + Administradores, sem herança) num arquivo
// sensível do Windows.
//
// O bit Unix (0600) não tem efeito nenhum sozinho em NTFS: o arquivo nasce com a ACL herdada do
// diretório, e o instalador concedia `users-full` nesse diretório — qualquer usuário local podia
// ler token.txt/config.json. `icacls` é o mesmo mecanismo que o instalador usa para conceder
// permissão (`[Dirs]` do .iss); aqui ele revoga: `/inheritance:r` corta a herança do diretório,
// `/grant:r` substitui (não soma) a lista por só SYSTEM e o grupo bem-conhecido de Administradores
// (`*S-1-5-32-544`, SID literal — não depende de idioma do Windows, ao contrário do nome
// "Administradores"/"Administrators").
func restrictFilePermissions(path string) error {
	cmd := hiddenCommand("icacls", path, "/inheritance:r", "/grant:r", "SYSTEM:F", "*S-1-5-32-544:F")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls %s: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
