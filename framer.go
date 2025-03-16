//простой HTTP/2 фреймер для работы с клиентскими http2 фреймами.
//документация в функциям написана DeepSeek
//проект реализован t.me/tokyoddos для Akira BotNet
//проект группы t.me/fuckenTOKYO
//Лицензия: GPL-3.0

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2/hpack"
)

// Framer представляет собой структуру для работы с HTTP/2 фреймами.
// Она содержит буферы для кодирования и декодирования HPACK, а также настройки для HTTP/2 соединения.
type Framer struct {
	hpackBuf               *bytes.Buffer
	hpackEnc               *hpack.Encoder
	hpackDec               *hpack.Decoder
	HEADER_TABLE_SIZE      uint32
	enable_push            uint32
	MAX_CONCURRENT_STREAMS uint32
	INITIAL_WINDOW_SIZE    uint32
	MAX_FRAME_SIZE         uint32
	MAX_HEADER_LIST_SIZE   uint32
	CONNECTION_FLOW        uint32
	lastStreamID           uint32
}

// Init инициализирует Framer, устанавливая значения по умолчанию для настроек HTTP/2 и инициализируя буферы.
func (f *Framer) Init() {
	var buf bytes.Buffer
	f.hpackBuf = &buf
	f.hpackEnc = hpack.NewEncoder(&buf)
	f.hpackDec = nil
	f.lastStreamID = 1

	f.HEADER_TABLE_SIZE = 65536
	f.enable_push = 0
	f.MAX_CONCURRENT_STREAMS = 0
	f.INITIAL_WINDOW_SIZE = 6291456
	f.MAX_FRAME_SIZE = 0
	f.MAX_HEADER_LIST_SIZE = 262144

	f.CONNECTION_FLOW = 15663105
}

// InitDecoder инициализирует декодер HPACK, если он еще не был инициализирован.
func (f *Framer) InitDecoder() {
	if f.hpackDec == nil {
		f.hpackDec = hpack.NewDecoder(f.HEADER_TABLE_SIZE, nil)
	}
}

// EnablePush устанавливает значение для параметра enable_push, который определяет, разрешены ли push-уведомления.
func (f *Framer) EnablePush(enable bool) {
	if enable {
		f.enable_push = uint32(1)
	} else {
		f.enable_push = uint32(0)
	}
}

// encUint16 кодирует uint16 в байтовый срез в формате BigEndian.
func encUint16(value uint16) []byte {
	buf := make([]byte, 2)
	binary.BigEndian.PutUint16(buf, value)
	return buf
}

// encUint32 кодирует uint32 в байтовый срез в формате BigEndian.
func encUint32(value uint32) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, value)
	return buf
}

// encHeader создает заголовок HTTP/2 фрейма с указанными параметрами.
func encHeader(len uint32, frameType, flags uint8, streamID uint32) []byte {
	buf := make([]byte, 9)
	binary.BigEndian.AppendUint32(buf[:4], len<<8)
	buf[3] = frameType
	buf[4] = flags
	binary.BigEndian.AppendUint32(buf[5:], streamID)
	return buf
}

// HeadersFrame создает фрейм заголовков HTTP/2 с указанными параметрами.
// Возвращает байтовый срез, представляющий фрейм, или ошибку в случае неудачи.
func (f *Framer) HeadersFrame(
	streamID uint32,
	headers [][2]string,
	endHeaders bool,
	endStream bool,
	payload ...string,
) ([]byte, error) {
	if streamID == 0 {
		streamID = atomic.LoadUint32(&f.lastStreamID)
		atomic.AddUint32(&f.lastStreamID, 2)
	}
	f.hpackBuf.Reset()

	for _, header := range headers {
		err := f.hpackEnc.WriteField(hpack.HeaderField{
			Name:  header[0],
			Value: header[1],
		})
		if err != nil {
			return nil, fmt.Errorf("failed to hpack headers")
		}
	}
	headersPayload := f.hpackBuf.Bytes()

	flags := uint8(0)
	if endHeaders {
		flags |= 0x4
	}
	if endStream {
		flags |= 0x1
	}

	var payloadBytes []byte
	if len(payload) > 0 {
		for _, p := range payload {
			payloadBytes = append(payloadBytes, []byte(p)...)
		}
	}
	frame := encHeader(uint32(len(headersPayload)+len(payloadBytes)), 0x1, flags, streamID)
	frame = append(frame, headersPayload...)
	frame = append(frame, payloadBytes...)
	return frame, nil
}

// SettingsFrame создает фрейм настроек HTTP/2 с текущими значениями настроек Framer.
func (f *Framer) SettingsFrame() []byte {
	var payload []byte
	if f.HEADER_TABLE_SIZE != 0 {
		payload = append(payload, encUint16(0x1)...)
		payload = append(payload, encUint32(f.HEADER_TABLE_SIZE)...)
	}
	payload = append(payload, encUint16(0x2)...)
	payload = append(payload, encUint32(f.enable_push)...)
	if f.MAX_CONCURRENT_STREAMS != 0 {
		payload = append(payload, encUint16(0x3)...)
		payload = append(payload, encUint32(f.MAX_CONCURRENT_STREAMS)...)
	}
	if f.INITIAL_WINDOW_SIZE != 0 {
		payload = append(payload, encUint16(0x4)...)
		payload = append(payload, encUint32(f.INITIAL_WINDOW_SIZE)...)
	}
	if f.MAX_FRAME_SIZE != 0 {
		payload = append(payload, encUint16(0x5)...)
		payload = append(payload, encUint32(f.MAX_FRAME_SIZE)...)
	}
	if f.MAX_HEADER_LIST_SIZE != 0 {
		payload = append(payload, encUint16(0x6)...)
		payload = append(payload, encUint32(f.MAX_HEADER_LIST_SIZE)...)
	}
	frame := encHeader(uint32(len(payload)), 0x4, 0x0, 0)
	frame = append(frame, payload...)
	return frame
}

// UpdateWindowFrame создает фрейм обновления окна HTTP/2 с текущим значением CONNECTION_FLOW.
func (f *Framer) UpdateWindowFrame() []byte {
	payload := encUint32(f.CONNECTION_FLOW)
	frame := encHeader(uint32(len(payload)), 0x8, 0x0, 0)
	frame = append(frame, payload...)
	return frame
}

// InitFrames создает и возвращает байтовый срез, содержащий начальные фреймы (настройки и обновление окна).
func (f *Framer) InitFrames() []byte {
	var payload []byte
	payload = append(payload, f.SettingsFrame()...)
	payload = append(payload, f.UpdateWindowFrame()...)
	return payload
}

// Handshake выполняет рукопожатие HTTP/2, отправляя преамбулу и начальные фреймы на сервер.
// Возвращает ошибку в случае неудачи.
func (f *Framer) Handshake(sock net.Conn, timeout time.Duration) error {
	if _, err := sock.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")); err != nil {
		return fmt.Errorf("failed to send http2 preface")
	}
	if _, err := sock.Write(f.InitFrames()); err != nil {
		return fmt.Errorf("failed to send initial frames (settings&update_window) to socket")
	}
	sock.SetReadDeadline(time.Now().Add(timeout))
	res := make([]byte, 1024)
	n, err := sock.Read(res)
	if err != nil {
		return fmt.Errorf("failed to read settings frame from server")
	}
	if n < 9 {
		return fmt.Errorf("server settings too short")
	}
	if res[3] != 0x4 || res[4]&0x1 == 0 {
		return fmt.Errorf("invalid answer from server")
	}
	if _, err := sock.Write([]byte{0x00, 0x00, 0x00, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00}); err != nil {
		return fmt.Errorf("failed to send ACK, but settings frame was valid")
	}
	return nil
}

// StatusCode извлекает код состояния HTTP из фрейма заголовков.
// Возвращает код состояния или ошибку, если фрейм недействителен или код состояния не найден.
func (f *Framer) StatusCode(frame *[]byte) (int, error) {
	if frame == nil || len(*frame) < 9 {
		return 0, fmt.Errorf("invalid frame provided")
	}
	if (*frame)[3] != 0x1 {
		return 0, fmt.Errorf("invalid frame provided")
	}
	if f.hpackDec == nil {
		f.InitDecoder()
	}
	payload := (*frame)[:9]

	headers, err := f.hpackDec.DecodeFull(payload)
	if err != nil {
		return 0, fmt.Errorf("failed to decode headers")
	}
	for _, hf := range headers {
		if hf.Name == ":status" {
			var statusCode int
			_, err := fmt.Sscanf(hf.Value, "%d", &statusCode)
			if err != nil {
				return 0, fmt.Errorf("failed to parse status code")
			}
			return statusCode, nil
		}
	}
	return 0, fmt.Errorf("no status pseudo header")
}

// GetHeaders извлекает заголовки из фрейма заголовков HTTP/2.
// Возвращает срез пар "ключ-значение" или ошибку, если фрейм недействителен.
func (f *Framer) GetHeaders(frame *[]byte) ([][2]string, error) {
	if frame == nil || len(*frame) < 9 {
		return nil, fmt.Errorf("invalid frame provided")
	}
	if (*frame)[3] != 0x1 {
		return nil, fmt.Errorf("invalid frame provided")
	}
	if f.hpackDec == nil {
		f.InitDecoder()
	}
	payload := (*frame)[:9]

	headers, err := f.hpackDec.DecodeFull(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to decode headers")
	}
	var res [][2]string
	for _, hf := range headers {
		res = append(res, [2]string{hf.Name, hf.Value})
	}
	return res, nil
}
