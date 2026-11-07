package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// maxUsageScanBytes - сколько байт ответа (или одной строки потока) просматривать в поисках usage
const maxUsageScanBytes = 1 << 20

// usage - счетчики токенов из ответа модели в формате OpenAI
type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// copyResult - итог передачи ответа: найденный usage и ошибка, если передача прервалась
type copyResult struct {
	usage usage
	err   error
}

// copyResponse - передает клиенту код, тип и тело ответа апстрима и по пути находит usage;
// поток SSE отправляется кусками сразу, firstChunk вызывается после первого отправленного куска
func copyResponse(w http.ResponseWriter, resp *http.Response, firstChunk func()) copyResult {
	contentType := resp.Header.Get("Content-Type")
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	streaming := strings.HasPrefix(contentType, "text/event-stream")
	if streaming {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.StatusCode)

	if !streaming {
		capture := &cappedBuffer{limit: maxUsageScanBytes}
		_, err := io.Copy(w, io.TeeReader(resp.Body, capture))
		return copyResult{usage: capture.usage(), err: err}
	}

	scanner := &sseUsage{}
	controller := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	sent := false
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return copyResult{usage: scanner.found, err: err}
			}
			if err := controller.Flush(); err != nil {
				return copyResult{usage: scanner.found, err: err}
			}
			if !sent {
				sent = true
				firstChunk()
			}
			scanner.feed(buf[:n])
		}
		if errors.Is(readErr, io.EOF) {
			return copyResult{usage: scanner.found}
		}
		if readErr != nil {
			return copyResult{usage: scanner.found, err: readErr}
		}
	}
}

// cappedBuffer - io.Writer, который запоминает не больше limit байт и помнит, что ответ не поместился
type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

// Write - сохраняет данные, пока не превышен лимит; никогда не возвращает ошибку, чтобы не сломать передачу клиенту
func (c *cappedBuffer) Write(p []byte) (int, error) {
	if !c.overflow && c.buf.Len()+len(p) <= c.limit {
		c.buf.Write(p)
	} else {
		c.overflow = true
	}
	return len(p), nil
}

// usage - usage из сохраненного JSON-ответа, нули, если ответ не поместился или не разобрался
func (c *cappedBuffer) usage() usage {
	if c.overflow {
		return usage{}
	}
	var body struct {
		Usage usage `json:"usage"`
	}
	_ = json.Unmarshal(c.buf.Bytes(), &body)
	return body.Usage
}

// sseUsage - ищет usage в строках потока "data: {...}", куски потока могут резать строки где угодно
type sseUsage struct {
	line  []byte
	skip  bool
	found usage
}

// feed - разбирает очередной кусок потока на строки
func (s *sseUsage) feed(chunk []byte) {
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			s.append(chunk)
			return
		}
		s.append(chunk[:i])
		s.endLine()
		chunk = chunk[i+1:]
	}
}

// append - копит текущую строку, слишком длинную строку пропускает целиком
func (s *sseUsage) append(p []byte) {
	if s.skip || len(s.line)+len(p) > maxUsageScanBytes {
		s.skip = true
		s.line = s.line[:0]
		return
	}
	s.line = append(s.line, p...)
}

// endLine - строка закончилась: если в ней есть usage, запоминаем его
func (s *sseUsage) endLine() {
	line := bytes.TrimSpace(s.line)
	skip := s.skip
	s.line, s.skip = s.line[:0], false
	if skip || !bytes.HasPrefix(line, []byte("data:")) || !bytes.Contains(line, []byte(`"usage"`)) {
		return
	}

	var chunk struct {
		Usage *usage `json:"usage"`
	}
	if json.Unmarshal(bytes.TrimSpace(line[len("data:"):]), &chunk) == nil && chunk.Usage != nil {
		s.found = *chunk.Usage
	}
}
