package dashboard

import (
	"bufio"
	"encoding/binary"
	"image"
	"io"
	"os"
)

// Ảnh iPhone thường lưu pixel theo hướng cảm biến và ghi hướng hiển thị vào EXIF
// Orientation (6 = xoay 90° theo chiều kim đồng hồ). Decoder của Go bỏ qua EXIF,
// nên mọi phép đo kích thước và thumbnail phải tự áp hướng này — nếu không, ảnh
// dọc sẽ hiện nằm ngang.

// jpegOrientation trả EXIF Orientation (1–8) của một JPEG; 1 nếu không có/không đọc được.
// Chỉ đọc các segment header tới SOS, không đọc dữ liệu ảnh.
func jpegOrientation(r io.Reader) int {
	br := bufio.NewReader(r)
	var soi [2]byte
	if _, err := io.ReadFull(br, soi[:]); err != nil || soi[0] != 0xFF || soi[1] != 0xD8 {
		return 1
	}
	for {
		b, err := br.ReadByte()
		if err != nil || b != 0xFF {
			return 1
		}
		m, err := br.ReadByte()
		for err == nil && m == 0xFF { // byte đệm 0xFF
			m, err = br.ReadByte()
		}
		if err != nil || m == 0xDA || m == 0xD9 { // SOS/EOI: hết vùng header
			return 1
		}
		var lb [2]byte
		if _, err := io.ReadFull(br, lb[:]); err != nil {
			return 1
		}
		segLen := int(binary.BigEndian.Uint16(lb[:])) - 2
		if segLen < 0 {
			return 1
		}
		if m == 0xE1 { // APP1 — có thể là EXIF (tối đa ~64KB)
			seg := make([]byte, segLen)
			if _, err := io.ReadFull(br, seg); err != nil {
				return 1
			}
			if o := exifOrientation(seg); o > 0 {
				return o
			}
			continue
		}
		if _, err := br.Discard(segLen); err != nil {
			return 1
		}
	}
}

// exifOrientation đọc tag 0x0112 trong IFD0 của một segment APP1 "Exif\0\0".
// Trả 0 nếu segment không phải EXIF hoặc không có tag.
func exifOrientation(seg []byte) int {
	if len(seg) < 14 || string(seg[:6]) != "Exif\x00\x00" {
		return 0
	}
	t := seg[6:]
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 0
	}
	n := int(bo.Uint16(t[off : off+2]))
	for i := 0; i < n; i++ {
		e := off + 2 + 12*i
		if e+12 > len(t) {
			return 0
		}
		if bo.Uint16(t[e:e+2]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8 : e+10])); v >= 1 && v <= 8 {
				return v
			}
			return 0
		}
	}
	return 0
}

// swapsAxes cho biết hướng EXIF có đổi chỗ chiều rộng/cao không (xoay 90°/270°).
func swapsAxes(orientation int) bool { return orientation >= 5 && orientation <= 8 }

// applyOrientation trả ảnh đã xoay/lật theo EXIF Orientation. Chỉ gọi trên ảnh
// nhỏ (thumbnail) vì duyệt từng pixel.
func applyOrientation(src *image.RGBA, o int) *image.RGBA {
	if o <= 1 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if swapsAxes(o) {
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2: // lật ngang
				dx, dy = w-1-x, y
			case 3: // xoay 180°
				dx, dy = w-1-x, h-1-y
			case 4: // lật dọc
				dx, dy = x, h-1-y
			case 5: // transpose
				dx, dy = y, x
			case 6: // xoay 90° theo chiều kim đồng hồ
				dx, dy = h-1-y, x
			case 7: // transverse
				dx, dy = h-1-y, w-1-x
			case 8: // xoay 90° ngược chiều kim đồng hồ
				dx, dy = y, w-1-x
			}
			dst.SetRGBA(dx, dy, src.RGBAAt(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

// imageMeta đọc kích thước pixel (chưa xoay) và hướng EXIF của file ảnh mà
// KHÔNG decode pixel. f được trả về vị trí đầu file.
func imageMeta(f *os.File) (cfg image.Config, format string, orientation int, err error) {
	cfg, format, err = image.DecodeConfig(f)
	if err != nil {
		return cfg, format, 1, err
	}
	orientation = 1
	if format == "jpeg" {
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return cfg, format, 1, err
		}
		orientation = jpegOrientation(f)
	}
	_, err = f.Seek(0, io.SeekStart)
	return cfg, format, orientation, err
}
