package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Log em arquivo do agente — motivado pela caçada do bug do subscribe (2026-08-12): o `logf`
// só escrevia em stdout, que o serviço do Windows descarta, e um defeito que o agente anunciava
// numa linha (`[EVENTS] sessão encerrada: UnsupportedOperation`) custou uma investigação inteira
// de métricas server-side + sondas de protocolo. Em campo (escola, sem acesso à AWS), o arquivo
// é a ÚNICA testemunha local.
//
// Desenho mínimo, sem dependência nova:
//   - mesmo diretório de dados da config (`configDataDir()` — `%ProgramData%\GestorEscolar` no
//     Windows, `/var/lib/gestor-escolar` no Linux), arquivo `agent.log`;
//   - rotação por tamanho: passou de `logFileMaxBytes`, vira `agent.log.1` (1 backup, disco
//     limitado a ~2× o teto);
//   - falha de escrita/abertura NUNCA derruba nem silencia o agente: stdout continua sempre, o
//     arquivo é best-effort.
const (
	logFileName     = "agent.log"
	logFileBackup   = "agent.log.1"
	logFileMaxBytes = 5 * 1024 * 1024
)

// fileLogSink serializa a escrita (logf é chamado de várias goroutines: transporte, jobs,
// updater, painel) e mantém o tamanho corrente para decidir a rotação sem stat por linha.
type fileLogSink struct {
	mu       sync.Mutex
	path     string
	backup   string
	maxBytes int64
	f        *os.File
	size     int64
}

// newFileLogSink abre (ou cria) o arquivo em append. Erro devolve nil — chamador ignora e o
// agente segue só com stdout.
func newFileLogSink(dir string, maxBytes int64) *fileLogSink {
	path := filepath.Join(dir, logFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil
	}
	size := int64(0)
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}
	return &fileLogSink{
		path:     path,
		backup:   filepath.Join(dir, logFileBackup),
		maxBytes: maxBytes,
		f:        f,
		size:     size,
	}
}

// write acrescenta uma linha já formatada, rotacionando antes se ela estouraria o teto.
func (s *fileLogSink) write(line string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return
	}
	if s.size+int64(len(line)) > s.maxBytes {
		s.rotateLocked()
	}
	if n, err := s.f.WriteString(line); err == nil {
		s.size += int64(n)
	}
}

// rotateLocked fecha o arquivo atual, promove para `.1` (substituindo o backup anterior) e
// recomeça vazio. Qualquer erro degrada para "continua escrevendo no arquivo atual" — rotação é
// conforto, perder log é o que não pode.
func (s *fileLogSink) rotateLocked() {
	_ = s.f.Close()
	_ = os.Remove(s.backup)
	_ = os.Rename(s.path, s.backup)
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		// Reabre o antigo (renomeado ou não) para não perder linhas futuras.
		if old, err2 := os.OpenFile(s.backup, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err2 == nil {
			s.f = old
			if st, err3 := old.Stat(); err3 == nil {
				s.size = st.Size()
			}
			return
		}
		s.f = nil
		return
	}
	s.f = f
	s.size = 0
}

// logSink é o destino global usado por logf. Nil até initFileLog — e possivelmente para sempre,
// se o disco recusar: o agente nunca condiciona nada ao log em arquivo.
var logSink *fileLogSink

// initFileLog liga o espelho em disco do logf. Chamado uma vez, cedo no main do serviço.
func initFileLog() {
	logSink = newFileLogSink(configDataDir(), logFileMaxBytes)
	if logSink == nil {
		fmt.Printf("aviso: log em arquivo indisponível (sem permissão de escrita?); seguindo só com stdout\n")
	}
}
