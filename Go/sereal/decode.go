package sereal

import (
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"strings"
)

type serealHeader struct {
	doctype     documentType
	version     byte
	suffixStart int
	suffixSize  int
	suffixFlags uint8
}

func readHeader(b []byte) (serealHeader, error) {

	if len(b) <= headerSize {
		return serealHeader{}, ErrBadHeader
	}

	first4Bytes := binary.LittleEndian.Uint32(b[:4])

	var h serealHeader

	h.doctype = documentType(b[4] >> 4)
	h.version = b[4] & 0x0f

	validHeader := false

	switch first4Bytes {
	case magicHeaderBytes:
		if 1 <= h.version && h.version <= 2 {
			validHeader = true
		}
	case magicHeaderBytesHighBit:
		if h.version >= 3 {
			validHeader = true
		}
	case magicHeaderBytesHighBitUTF8:
		return serealHeader{}, ErrBadHeaderUTF8
	}

	if !validHeader {
		return serealHeader{}, ErrBadHeader
	}

	ln, sz, err := varintdecode(b[5:])
	if err != nil {
		return serealHeader{}, err
	}

	h.suffixSize = ln + sz
	h.suffixStart = headerSize + sz

	return h, nil
}

// LooksLikeSereal perofrms a quick and rudimentary check whether the buffer contains a Sereal document
func LooksLikeSereal(b []byte) bool {
	if len(b) < 7 {
		return false
	}

	_, err := readHeader(b)

	return err == nil
}

// A Decoder reads and decodes Sereal objects from an input buffer
type Decoder struct {
	tracked   map[int]reflect.Value
	umcache   map[string]reflect.Type
	tcache    tagsCache
	copyDepth int

	PerlCompat bool
}

type decompressor interface {
	decompress(d, b []byte) ([]byte, error)
}

// NewDecoder returns a decoder with default flags
func NewDecoder() *Decoder {
	return &Decoder{}
}

// Unmarshal decodes b into body with the default decoder
func Unmarshal(b []byte, body interface{}) error {
	decoder := &Decoder{}
	return decoder.UnmarshalHeaderBody(b, nil, body)
}

// UnmarshalHeader parses the Sereal-v2-encoded buffer b and stores the header data into the variable pointed to by vheader
func (d *Decoder) UnmarshalHeader(b []byte, vheader interface{}) (err error) {
	return d.UnmarshalHeaderBody(b, vheader, nil)
}

// Unmarshal parses the Sereal-encoded buffer b and stores the result in the value pointed to by vbody
func (d *Decoder) Unmarshal(b []byte, vbody interface{}) (err error) {
	return d.UnmarshalHeaderBody(b, nil, vbody)
}

func checkHeader(b []byte) (serealHeader, error) {
	header, err := readHeader(b)
	if err != nil {
		return header, err
	}

	switch header.version {
	case 1:
		break
	case 2:
		break
	case 3:
		break
	case 4:
		break
	default:
		return header, fmt.Errorf("document version '%d' not yet supported", header.version)
	}

	return header, nil
}

func documentDecompressor(version byte, doctype documentType) (decompressor, error) {
	var decomp decompressor

	switch doctype {
	case serealRaw:
		// nothing

	case serealSnappy:
		if version != 1 {
			return nil, ErrBadSnappy
		}
		decomp = SnappyCompressor{Incremental: false}

	case serealSnappyIncremental:
		decomp = SnappyCompressor{Incremental: true}

	case serealZlib:
		if version < 3 {
			return nil, ErrBadZlibV3
		}
		decomp = ZlibCompressor{}

	case serealZstd:
		if version < 4 {
			return nil, ErrBadZstdV4
		}
		decomp = ZstdCompressor{}

	default:
		return nil, fmt.Errorf("document type '%d' not yet supported", doctype)
	}

	return decomp, nil
}

// UnmarshalHeaderBody parses the Sereal-encoded buffer b extracts the header and body data into vheader and vbody, respectively
func (d *Decoder) UnmarshalHeaderBody(b []byte, vheader interface{}, vbody interface{}) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(runtime.Error); ok {
				panic(r)
			}

			switch t := r.(type) {
			case string:
				err = errors.New(t)
			case error:
				err = t
			}
		}
	}()

	header, err := checkHeader(b)
	if err != nil {
		return err
	}

	decomp, err := documentDecompressor(header.version, header.doctype)
	if err != nil {
		return err
	}

	bodyStart := headerSize + header.suffixSize

	if bodyStart > len(b) || bodyStart < 0 {
		return ErrCorrupt{errBadOffset}
	}

	if vheader != nil && header.suffixSize != 1 {
		d.tracked = make(map[int]reflect.Value)
		defer func() { d.tracked = nil }()

		headerValue := reflect.ValueOf(vheader)
		if headerValue.Kind() != reflect.Ptr {
			return ErrHeaderPointer
		}

		header.suffixFlags = b[header.suffixStart]
		if header.suffixFlags&1 == 1 {
			if ptr, ok := vheader.(*interface{}); ok && *ptr == nil {
				_, err = d.decode(b[:bodyStart], header.suffixStart+1, ptr)
			} else {
				_, err = d.decodeViaReflection(b[:bodyStart], header.suffixStart+1, headerValue.Elem())
			}
		}
	}

	if err == nil && vbody != nil {
		/* XXX instead of creating an uncompressed copy of the document,
		 *     it would be more flexible to use a sort of "Reader" interface */
		if decomp != nil {
			decompBody, err := decomp.decompress(nil, b[bodyStart:])
			if err != nil {
				return err
			}

			newBody := make([]byte, 0, len(b[:bodyStart])+len(decompBody))
			newBody = append(newBody, b[:bodyStart]...)
			newBody = append(newBody, decompBody...)
			b = newBody
		}

		d.tracked = make(map[int]reflect.Value)
		defer func() { d.tracked = nil }()

		bodyValue := reflect.ValueOf(vbody)
		if bodyValue.Kind() != reflect.Ptr {
			return ErrBodyPointer
		}

		if header.version == 1 {
			if ptr, ok := vbody.(*interface{}); ok && *ptr == nil {
				_, err = d.decode(b, bodyStart, ptr)
			} else {
				_, err = d.decodeViaReflection(b, bodyStart, bodyValue.Elem())
			}
		} else {
			// serealv2 documents have 1-based offsets :/
			if ptr, ok := vbody.(*interface{}); ok && *ptr == nil {
				_, err = d.decode(b[bodyStart-1:], 1, ptr)
			} else {
				_, err = d.decodeViaReflection(b[bodyStart-1:], 1, bodyValue.Elem())
			}
		}
	}

	return err
}

/****************************************************************
 * Decode document of unknown structure (i.e. without reflection)
 ****************************************************************/
func (d *Decoder) decode(by []byte, idx int, ptr *interface{}) (int, error) {
	if idx < 0 || idx >= len(by) {
		return 0, ErrTruncated
	}

	tag := by[idx]

	// skip over any padding bytes
	for tag == typePAD || tag == typePAD|trackFlag {
		idx++
		if idx >= len(by) {
			return 0, ErrTruncated
		}

		tag = by[idx]
	}

	trackme := (tag & trackFlag) == trackFlag
	if trackme {
		tag &^= trackFlag
		d.tracked[idx] = reflect.ValueOf(ptr)
	}

	//fmt.Printf("start decode: tag %d (0x%x) at %d\n", int(tag), int(tag), idx)
	idx++

	var err error
	switch {
	case tag < typeVARINT:
		*ptr = d.decodeInt(tag)

	case tag == typeVARINT:
		var val int
		if val, idx, err = d.decodeVarint(by, idx); err != nil {
			return 0, err
		}
		if val < 0 {
			*ptr = uint(val)
		} else {
			*ptr = val
		}

	case tag == typeZIGZAG:
		*ptr, idx, err = d.decodeZigzag(by, idx)

	case tag == typeFLOAT:
		*ptr, idx, err = d.decodeFloat(by, idx)

	case tag == typeDOUBLE:
		*ptr, idx, err = d.decodeDouble(by, idx)

	case tag == typeTRUE:
		*ptr = true

	case tag == typeFALSE:
		*ptr = false

	case tag == typeHASH:
		// see commends at top of the function
		var ln, sz int
		ln, sz, err = varintdecode(by[idx:])
		if err != nil {
			return 0, err
		}
		idx, err = d.decodeHash(by, idx+sz, ln, ptr, false)

	case tag >= typeHASHREF_0 && tag < typeHASHREF_0+16:
		idx, err = d.decodeHash(by, idx, int(tag&0x0f), ptr, d.PerlCompat)
		if err != nil {
			return 0, err
		}

	case tag == typeARRAY:
		// see commends at top of the function
		var ln, sz int
		ln, sz, err = varintdecode(by[idx:])
		if err != nil {
			return 0, err
		}
		idx, err = d.decodeArray(by, idx+sz, ln, ptr, false)
		if err != nil {
			return 0, err
		}

	case tag >= typeARRAYREF_0 && tag < typeARRAYREF_0+16:
		idx, err = d.decodeArray(by, idx, int(tag&0x0f), ptr, d.PerlCompat)
		if err != nil {
			return 0, err
		}

	case tag == typeSTR_UTF8:
		var val []byte
		var ln, sz int
		ln, sz, err = varintdecode(by[idx:])
		if err != nil {
			return 0, err
		}
		if val, idx, err = d.decodeBinary(by, idx+sz, ln, false); err != nil {
			return 0, err
		}
		*ptr = string(val)

	case tag == typeBINARY:
		var ln, sz int
		ln, sz, err = varintdecode(by[idx:])
		if err != nil {
			return 0, err
		}
		*ptr, idx, err = d.decodeBinary(by, idx+sz, ln, true)
		if err != nil {
			return 0, err
		}

	case tag >= typeSHORT_BINARY_0 && tag < typeSHORT_BINARY_0+32:
		*ptr, idx, err = d.decodeBinary(by, idx, int(tag&0x1f), true)
		if err != nil {
			return 0, err
		}

	case tag == typeUNDEF, tag == typeCANONICAL_UNDEF:
		if d.PerlCompat && tag == typeCANONICAL_UNDEF {
			*ptr = perlCanonicalUndef
		} else if d.PerlCompat {
			*ptr = &PerlUndef{}
		} else {
			*ptr = nil
		}

	case tag == typeCOPY:
		if d.copyDepth > 0 {
			return 0, ErrCorrupt{errNestedCOPY}
		}

		var offs, sz int
		offs, sz, err = varintdecode(by[idx:])
		if err != nil {
			return 0, err
		}
		if offs < 0 || offs >= idx {
			return 0, ErrCorrupt{errBadOffset}
		}
		idx += sz

		d.copyDepth++
		_, err = d.decode(by, offs, ptr)
		d.copyDepth--

	case tag == typeREFN:
		if d.PerlCompat {
			var iface interface{}
			*ptr = &iface

			if idx, err = d.decode(by, idx, &iface); !trackme && err == nil {
				// if REFN is not tracked, build a pointer to concrete type
				// otherwise let ptr be pointer to something (i.e. *interface{})
				riface := reflect.ValueOf(iface)

				// reflect.New create value of type *riface.Type())
				val := reflect.New(riface.Type())
				val.Elem().Set(riface)
				*ptr = val.Interface()
			}
		} else {
			idx, err = d.decode(by, idx, ptr)
		}

	case tag == typeREFP, tag == typeALIAS:
		var val reflect.Value
		if val, idx, err = d.decodeREFP_ALIAS(by, idx, tag == typeREFP); err == nil {
			*ptr = val.Interface()
		}

	case tag == typeWEAKEN:
		if d.PerlCompat {
			pweak := PerlWeakRef{}
			*ptr = &pweak
			idx, err = d.decode(by, idx, &pweak.Reference)
		} else {
			idx, err = d.decode(by, idx, ptr)
		}

	case tag == typeREGEXP:
		*ptr, idx, err = d.decodeRegexp(by, idx)

	case tag == typeOBJECT, tag == typeOBJECTV:
		rvPtr := reflect.ValueOf(ptr)
		idx, err = d.decodeObjectViaReflection(by, idx, rvPtr.Elem(), tag == typeOBJECTV)

	case tag == typeOBJECT_FREEZE, tag == typeOBJECTV_FREEZE:
		rvPtr := reflect.ValueOf(ptr)
		idx, err = d.decodeObjectFreezeViaReflection(by, idx, rvPtr.Elem(), tag == typeOBJECTV_FREEZE)

	default:
		return 0, fmt.Errorf("unknown tag byte: %d (0x%x)", int(tag), int(tag))
	}

	//fmt.Printf("stop decode: tag %d (0x%x)\n", int(tag), int(tag))
	return idx, err
}

func (d *Decoder) decodeInt(tag byte) int {
	if (tag & 0x10) == 0x10 {
		return int(tag) - 32 // negative number
	}
	return int(tag)
}

func (d *Decoder) decodeVarint(by []byte, idx int) (int, int, error) {
	i, sz, err := varintdecode(by[idx:])
	return i, idx + sz, err
}

func (d *Decoder) decodeZigzag(by []byte, idx int) (int, int, error) {
	i, sz, err := varintdecode(by[idx:])
	return int(-(1 + (uint64(i) >> 1))), idx + sz, err
}

func (d *Decoder) decodeFloat(by []byte, idx int) (float32, int, error) {
	if idx+3 >= len(by) {
		return 0, 0, ErrTruncated
	}

	bits := uint32(by[idx]) | uint32(by[idx+1])<<8 | uint32(by[idx+2])<<16 | uint32(by[idx+3])<<24
	return math.Float32frombits(bits), idx + 4, nil
}

func (d *Decoder) decodeDouble(by []byte, idx int) (float64, int, error) {
	if idx+7 >= len(by) {
		return 0, 0, ErrTruncated
	}

	bits := uint64(by[idx]) | uint64(by[idx+1])<<8 | uint64(by[idx+2])<<16 | uint64(by[idx+3])<<24 | uint64(by[idx+4])<<32 | uint64(by[idx+5])<<40 | uint64(by[idx+6])<<48 | uint64(by[idx+7])<<56
	return math.Float64frombits(bits), idx + 8, nil
}

func (d *Decoder) decodeHash(by []byte, idx int, ln int, ptr *interface{}, isRef bool) (int, error) {
	if ln < 0 || ln > math.MaxInt32 {
		return 0, ErrCorrupt{errBadHashSize}
	}

	if idx+2*ln > len(by) {
		return 0, ErrTruncated
	}

	hash := make(map[string]interface{}, ln)

	if isRef {
		*ptr = &hash
	} else {
		*ptr = hash
	}

	var err error
	for i := 0; i < ln; i++ {
		var key []byte
		key, idx, err = d.decodeStringish(by, idx)
		if err != nil {
			return 0, err
		}

		var value interface{}
		idx, err = d.decode(by, idx, &value)
		if err != nil {
			return 0, err
		}

		hash[string(key)] = value
	}

	return idx, nil
}

func (d *Decoder) decodeArray(by []byte, idx int, ln int, ptr *interface{}, isRef bool) (int, error) {
	if ln < 0 || ln > math.MaxInt32 {
		return 0, ErrCorrupt{errBadSliceSize}
	}

	if idx+ln > len(by) {
		return 0, ErrTruncated
	}

	var slice []interface{}

	if ln == 0 {
		// FIXME this is not optimal
		slice = make([]interface{}, 0, 1)
	} else {
		slice = make([]interface{}, ln)
	}

	if isRef {
		*ptr = &slice
	} else {
		*ptr = slice
	}

	var err error
	for i := 0; i < ln; i++ {
		idx, err = d.decode(by, idx, &slice[i])
		if err != nil {
			return 0, err
		}
	}

	return idx, nil
}

func (d *Decoder) decodeBinary(by []byte, idx int, ln int, makeCopy bool) ([]byte, int, error) {
	if ln < 0 || ln > math.MaxInt32 {
		return nil, 0, ErrCorrupt{errBadStringSize}
	}
	if idx+ln > len(by) {
		return nil, 0, ErrTruncated
	}

	if makeCopy {
		res := make([]byte, ln)
		copy(res, by[idx:idx+ln])
		return res, idx + ln, nil
	}

	return by[idx : idx+ln], idx + ln, nil
}

// decodeStringish() return slice of by, i.e. not a copy
func (d *Decoder) decodeStringish(by []byte, idx int) ([]byte, int, error) {
	if idx < 0 || idx >= len(by) {
		return nil, 0, ErrTruncated
	}

	//TODO trackme

	tag := by[idx]
	for tag == typePAD || tag == typePAD|trackFlag {
		idx++
		if idx >= len(by) {
			return nil, 0, ErrTruncated
		}

		tag = by[idx]
	}

	tag &^= trackFlag
	idx++

	//fmt.Printf("decodeStringish: tag %d (0x%x) at %d\n", int(tag), int(tag), idx)

	var res []byte
	switch {
	case tag == typeBINARY, tag == typeSTR_UTF8:
		ln, sz, err := varintdecode(by[idx:])
		if err != nil {
			return nil, 0, err
		}
		idx += sz

		if ln < 0 || ln > math.MaxInt32 {
			return nil, 0, ErrCorrupt{errBadStringSize}
		} else if idx+ln > len(by) {
			return nil, 0, ErrTruncated
		}

		res = by[idx : idx+ln]
		idx += ln

	case tag >= typeSHORT_BINARY_0 && tag < typeSHORT_BINARY_0+32:
		ln := int(tag & 0x1F) // get length from tag
		if idx+ln > len(by) {
			return nil, 0, ErrTruncated
		}

		res = by[idx : idx+ln]
		idx += ln

	case tag == typeCOPY:
		if d.copyDepth > 0 {
			return nil, 0, ErrCorrupt{errNestedCOPY}
		}

		offs, sz, err := varintdecode(by[idx:])
		if err != nil {
			return nil, 0, err
		}
		if offs < 0 || offs >= idx {
			return nil, 0, ErrCorrupt{errBadOffset}
		}
		idx += sz

		d.copyDepth++
		res, _, err = d.decodeStringish(by, offs)
		d.copyDepth--
		if err != nil {
			return nil, 0, err
		}

	default:
		return nil, 0, fmt.Errorf("expect stringish at offset %d but got %d (0x%x)", idx, int(tag), int(tag))
	}

	//fmt.Printf("decodeStringish res: %s at %d\n", string(res), idx)
	return res, idx, nil
}

func (d *Decoder) decodeRegexp(by []byte, idx int) (*PerlRegexp, int, error) {
	var err error
	var pattern []byte
	if pattern, idx, err = d.decodeStringish(by, idx); err != nil {
		return nil, 0, err
	}

	var modifiers []byte
	if modifiers, idx, err = d.decodeStringish(by, idx); err != nil {
		return nil, 0, err
	}

	// TODO perhaps, copy values
	return &PerlRegexp{pattern, modifiers}, idx, nil
}

/********************************************************************
 * Decode document with predefined structure (have to use reflection)
 ********************************************************************/
func (d *Decoder) decodeViaReflection(by []byte, idx int, ptr reflect.Value) (int, error) {
	if idx < 0 || idx >= len(by) {
		return 0, ErrTruncated
	}

	ptrKind := ptr.Kind()

	// at this point structure of decoding document is uknown, make a shortcut
	if ptrKind == reflect.Interface && ptr.IsNil() {
		var iface interface{}
		var err error
		idx, err = d.decode(by, idx, &iface)
		if iface != nil {
			ptr.Set(reflect.ValueOf(iface))
		}
		return idx, err
	}

	tag := by[idx]
	for tag == typePAD || tag == typePAD|trackFlag {
		idx++
		if idx >= len(by) {
			return 0, ErrTruncated
		}

		tag = by[idx]
	}

	if (tag & trackFlag) == trackFlag {
		tag &^= trackFlag
		d.tracked[idx] = ptr
	}

	//fmt.Printf("start decodeViaReflection: tag %d (0x%x) at %d\n", int(tag), int(tag), idx)
	idx++

	var err error
	switch {
	case tag < typeVARINT:
		setInt(ptr, d.decodeInt(tag))

	case tag == typeVARINT:
		var val int
		val, idx, err = d.decodeVarint(by, idx)
		if err != nil {
			return 0, err
		}
		setInt(ptr, val)

	case tag == typeZIGZAG:
		var val int
		val, idx, err = d.decodeZigzag(by, idx)
		if err != nil {
			return 0, err
		}
		setInt(ptr, val)

	case tag == typeFLOAT:
		var val float32
		if val, idx, err = d.decodeFloat(by, idx); err != nil {
			return 0, err
		}
		ptr.SetFloat(float64(val))

	case tag == typeDOUBLE:
		var val float64
		if val, idx, err = d.decodeDouble(by, idx); err != nil {
			return 0, err
		}
		ptr.SetFloat(val)

	case tag == typeTRUE, tag == typeFALSE:
		ptr.SetBool(tag == typeTRUE)

	case tag == typeBINARY:
		var val []byte
		var ln, sz int
		ln, sz, err = varintdecode(by[idx:])
		if err != nil {
			return 0, err
		}
		if val, idx, err = d.decodeBinary(by, idx+sz, ln, false); err != nil {
			return 0, err
		}
		setBinary(ptr, val)

	case tag >= typeSHORT_BINARY_0 && tag < typeSHORT_BINARY_0+32:
		var val []byte
		if val, idx, err = d.decodeBinary(by, idx, int(tag&0x1f), false); err != nil {
			return 0, err
		}
		setBinary(ptr, val)

	case tag == typeSTR_UTF8:
		var val []byte
		var ln, sz int
		ln, sz, err = varintdecode(by[idx:])
		if err != nil {
			return 0, err
		}
		if val, idx, err = d.decodeBinary(by, idx+sz, ln, false); err != nil {
			return 0, err
		}
		ptr.SetString(string(val))

	case tag == typeHASH:
		var ln, sz int
		ln, sz, err = varintdecode(by[idx:])
		if err != nil {
			return 0, err
		}
		idx, err = d.decodeHashViaReflection(by, idx+sz, ln, ptr)

	case tag >= typeHASHREF_0 && tag < typeHASHREF_0+16:
		idx, err = d.decodeHashViaReflection(by, idx, int(tag&0x0f), ptr)

	case tag == typeARRAY:
		var ln, sz int
		ln, sz, err = varintdecode(by[idx:])
		if err != nil {
			return 0, err
		}
		idx, err = d.decodeArrayViaReflection(by, idx+sz, ln, ptr)

	case tag >= typeARRAYREF_0 && tag < typeARRAYREF_0+16:
		idx, err = d.decodeArrayViaReflection(by, idx, int(tag&0x0f), ptr)

	case tag == typeUNDEF, tag == typeCANONICAL_UNDEF:
		if d.PerlCompat && tag == typeCANONICAL_UNDEF {
			ptr.Set(reflect.ValueOf(perlCanonicalUndef))
		} else if d.PerlCompat {
			ptr.Set(reflect.ValueOf(&PerlUndef{}))
		} else {
			if ptrKind == reflect.Ptr || ptrKind == reflect.Map || ptrKind == reflect.Slice {
				ptr.Set(reflect.Zero(ptr.Type()))
			} else {
				// maybe panic
			}
		}

	case tag == typeCOPY:
		if d.copyDepth > 0 {
			return 0, ErrCorrupt{errNestedCOPY}
		}

		var offs, sz int
		offs, sz, err = varintdecode(by[idx:])
		if err != nil {
			return 0, err
		}
		if offs < 0 || offs >= idx {
			return 0, ErrCorrupt{errBadOffset}
		}
		idx += sz

		d.copyDepth++
		_, err = d.decodeViaReflection(by, offs, ptr)
		d.copyDepth--

	case tag == typeREFN:
		idx, err = d.decodeViaReflection(by, idx, ptr)

	case tag == typeREFP, tag == typeALIAS:
		var val reflect.Value
		if val, idx, err = d.decodeREFP_ALIAS(by, idx, tag == typeREFP); err != nil {
			return 0, err

		}
		ptr.Set(val.Elem())

	case tag == typeWEAKEN:
		if d.PerlCompat {
			pweak := PerlWeakRef{}
			ptr.Set(reflect.ValueOf(&pweak))
			idx, err = d.decode(by, idx, &pweak.Reference)
		} else {
			idx, err = d.decodeViaReflection(by, idx, ptr)
		}

	case tag == typeREGEXP:
		var pregexp *PerlRegexp
		if pregexp, idx, err = d.decodeRegexp(by, idx); err != nil {
			return 0, err
		}
		ptr.Set(reflect.ValueOf(pregexp))

	case tag == typeOBJECT, tag == typeOBJECTV:
		idx, err = d.decodeObjectViaReflection(by, idx, ptr, tag == typeOBJECTV)

	case tag == typeOBJECT_FREEZE, tag == typeOBJECTV_FREEZE:
		idx, err = d.decodeObjectFreezeViaReflection(by, idx, ptr, tag == typeOBJECTV_FREEZE)

	default:
		return 0, fmt.Errorf("unknown tag byte: %d (0x%x)", int(tag), int(tag))
	}

	return idx, err
}

func (d *Decoder) decodeArrayViaReflection(by []byte, idx int, ln int, ptr reflect.Value) (int, error) {
	if ln < 0 || ln > math.MaxInt32 {
		return 0, ErrCorrupt{errBadSliceSize}
	}

	if idx+ln > len(by) {
		return 0, ErrTruncated
	}

	switch ptr.Kind() {
	case reflect.Slice:
		if ptr.IsNil() || ptr.Len() == 0 {
			ptr.Set(reflect.MakeSlice(ptr.Type(), ln, ln))
		}

	case reflect.Array:
		// do nothing

	default:
		return 0, &reflect.ValueError{Method: "sereal.decodeArrayViaReflection", Kind: ptr.Kind()}
	}

	var err error
	ptrLen := ptr.Len()

	for i := 0; i < ln; i++ {
		if i < ptrLen {
			idx, err = d.decodeViaReflection(by, idx, ptr.Index(i))
		} else {
			// we went outside of array length, so ignore folowwing content
			var iface interface{}
			idx, err = d.decode(by, idx, &iface) // TODO make this process to be efficient
		}

		if err != nil {
			return 0, err
		}
	}

	return idx, nil
}

func (d *Decoder) decodeHashViaReflection(by []byte, idx int, ln int, ptr reflect.Value) (int, error) {
	if ln < 0 || ln > math.MaxInt32 {
		return 0, ErrCorrupt{errBadHashSize}
	}

	if idx+2*ln > len(by) {
		return 0, ErrTruncated
	}

	switch ptr.Kind() {
	case reflect.Map:
		if ptr.IsNil() {
			ptr.Set(reflect.MakeMap(ptr.Type()))
		}

		var err error
		for i := 0; i < ln; i++ {
			var key []byte
			key, idx, err = d.decodeStringish(by, idx)
			if err != nil {
				return 0, err
			}

			keyValue := reflect.ValueOf(string(key))
			if value := ptr.MapIndex(keyValue); value.IsValid() {
				// strkey exists in map, replace its content but respect structure
				idx, err = d.decodeViaReflection(by, idx, value)
			} else {
				// there is no strkey in map, crete a new one
				riface := reflect.New(ptr.Type().Elem())
				idx, err = d.decodeViaReflection(by, idx, riface.Elem())
				if err != nil {
					return 0, err
				}

				ptr.SetMapIndex(keyValue, riface.Elem())
			}

			if err != nil {
				return 0, err
			}
		}

	case reflect.Ptr:
		if ptr.IsNil() {
			n := reflect.New(ptr.Type().Elem())
			ptr.Set(n)
		}

		return d.decodeHashViaReflection(by, idx, ln, ptr.Elem())
	case reflect.Struct:
		tags := d.tcache.Get(ptr)
		var err error
		for i := 0; i < ln; i++ {
			var key []byte
			key, idx, err = d.decodeStringish(by, idx)
			if err != nil {
				return 0, err
			}

			var fld tag
			var found bool

			if tags == nil {
				// do nothing
			} else if fld, found = tags[string(key)]; found {
				idx, err = d.decodeViaReflection(by, idx, ptr.Field(fld.id))
			} else if fld, found = tags[strings.Title(string(key))]; found {
				idx, err = d.decodeViaReflection(by, idx, ptr.Field(fld.id))
			}

			if !found {
				// struct doesn't contain field with strkey name
				var iface interface{}
				idx, err = d.decode(by, idx, &iface) // TODO make this process to be efficient
			}

			if err != nil {
				return 0, err
			}
		}

	default:
		return 0, &reflect.ValueError{Method: "sereal.decodeHashViaReflection", Kind: ptr.Kind()}
	}

	return idx, nil
}

func (d *Decoder) decodeREFP_ALIAS(by []byte, idx int, isREFP bool) (reflect.Value, int, error) {
	offs, sz, err := varintdecode(by[idx:])
	if err != nil {
		var res reflect.Value
		return res, 0, err
	}
	idx += sz

	if offs < 0 || offs >= idx {
		var res reflect.Value
		return res, 0, ErrCorrupt{errBadOffset}
	}

	rv, ok := d.tracked[offs]
	if !ok {
		var res reflect.Value
		var corrupt ErrCorrupt
		if isREFP {
			corrupt.Err = errUntrackedOffsetREFP
		} else {
			corrupt.Err = errUntrackedOffsetAlias
		}
		return res, 0, corrupt
	}

	var res reflect.Value
	if rv.Kind() == reflect.Ptr && rv.Elem().Kind() == reflect.Interface {
		// rv contains *interface{},
		// i.e. it was saved in decode() path
		// rv.Elem() will be an interface
		// rv.Elem().Elem() should be the data inside interface

		if isREFP {
			rvData := rv.Elem().Elem()
			res = reflect.New(rvData.Type())
			res.Elem().Set(rvData)
		} else {
			res = rv.Elem()
		}
	} else {
		// rv contains original value
		// i.e. it was saved in decodeViaReflection() path
		res = reflect.New(rv.Type())
		res.Elem().Set(rv)
	}

	return res, idx, nil
}

func (d *Decoder) decodeObjectViaReflection(by []byte, idx int, ptr reflect.Value, isObjectV bool) (int, error) {
	var err error
	var className []byte

	if !isObjectV {
		// typeOBJECT
		className, idx, err = d.decodeStringish(by, idx)
	} else {
		// typeOBJECTV
		var offs, sz int
		offs, sz, err = varintdecode(by[idx:])
		if err != nil {
			return 0, err
		}
		idx += sz
		className, _, err = d.decodeStringish(by, offs)
	}

	if err != nil {
		return 0, err
	}

	if d.PerlCompat {
		pobj := PerlObject{Class: string(className)}
		ptr.Set(reflect.ValueOf(&pobj))
		idx, err = d.decode(by, idx, &pobj.Reference)
	} else {
		// FIXME: stuff className somewhere if map/struct?
		idx, err = d.decodeViaReflection(by, idx, ptr)
	}

	return idx, err
}
func (d *Decoder) decodeObjectFreezeViaReflection(by []byte, idx int, ptr reflect.Value, isObjectV bool) (int, error) {
	var err error
	var className, classData []byte

	if !isObjectV {
		// typeOBJECT_FREEZE
		className, idx, err = d.decodeStringish(by, idx)
		if err != nil {
			return 0, err
		}
	} else {
		// typeOBJECTV_FREEZE
		offs, sz, err := varintdecode(by[idx:])
		if err != nil {
			return 0, err
		}
		idx += sz
		className, _, err = d.decodeStringish(by, offs)
		if err != nil {
			return 0, err
		}
	}

	if err != nil {
		return 0, err
	}

	if idx+1 >= len(by) {
		return 0, ErrTruncated
	}

	if by[idx] != typeREFN || by[idx+1] != typeARRAY {
		return 0, ErrCorrupt{errFreezeNotRefnArray}
	}

	var iface interface{}
	if idx, err = d.decode(by, idx, &iface); err != nil {
		return 0, err
	}

	wrapper, ok := iface.([]interface{})
	if !ok {
		return 0, ErrCorrupt{errFreezeNotArray}
	}

	if len(wrapper) != 1 {
		return 0, ErrCorrupt{errFreezeMultipleElts}
	}

	// Expecting a single item in the array ref
	if classData, ok = wrapper[0].([]byte); !ok {
		return 0, ErrCorrupt{errFreezeNotByteSlice}
	}

	strClassName := string(className)

	if d.PerlCompat {
		ptr.Set(reflect.ValueOf(&PerlFreeze{strClassName, classData}))
	} else {
		if obj, ok := findUnmarshaler(ptr); ok {

			if err := obj.UnmarshalBinary(classData); err != nil {
				return 0, err
			}
		} else {
			switch {
			case ptr.Kind() == reflect.Interface && ptr.IsNil():
				// do we have a registered handler for this type?
				concreteClass, ok := d.getUnmarshalerType(strClassName)

				if ok {
					rzero := instantiateZero(concreteClass)
					obj, ok := findUnmarshaler(rzero)

					if !ok {
						// only things that have an unmarshaler should have been put into the map
						panic(fmt.Sprintf("unable to find unmarshaler for %s", rzero))
					}

					if err := obj.UnmarshalBinary(classData); err != nil {
						return 0, err
					}

					ptr.Set(reflect.ValueOf(obj))
				} else {
					ptr.Set(reflect.ValueOf(&PerlFreeze{strClassName, classData}))
				}

			case ptr.Kind() == reflect.Slice && ptr.Type().Elem().Kind() == reflect.Uint8 && ptr.IsNil():
				ptr.Set(reflect.ValueOf(classData))

			default:
				return 0, fmt.Errorf("can't unpack FROZEN object into %v", ptr.Type())
			}
		}
	}

	return idx, err
}

func setInt(ptr reflect.Value, i int) {
	switch ptr.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		ptr.SetInt(int64(i))

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		ptr.SetUint(uint64(i))

	default:
		panic(&reflect.ValueError{Method: "sereal.setInt", Kind: ptr.Kind()})
	}
}

func setBinary(ptr reflect.Value, val []byte) {
	switch ptr.Kind() {
	case reflect.Slice:
		if ptr.Type().Elem().Kind() == reflect.Uint8 && ptr.IsNil() {
			slice := make([]byte, len(val))
			ptr.Set(reflect.ValueOf(slice))
		}

		reflect.Copy(ptr, reflect.ValueOf(val))

	case reflect.Array:
		reflect.Copy(ptr.Slice(0, ptr.Len()), reflect.ValueOf(val))

	case reflect.String:
		ptr.SetString(string(val))

	default:
		panic(&reflect.ValueError{Method: "sereal.setBinary", Kind: ptr.Kind()})
	}
}

func varintdecode(by []byte) (n int, sz int, err error) {
	s := uint(0) // shift count
	for i, b := range by {
		n |= int(b&0x7f) << s
		s += 7

		if (b & 0x80) == 0 {
			return n, i + 1, nil
		}

		if s > 63 {
			// too many continuation bits
			return 0, i + 1, ErrCorrupt{errBadVarint}
		}
	}

	// byte without continuation bit
	return 0, len(by), ErrCorrupt{errBadVarint}
}

func findUnmarshaler(ptr reflect.Value) (encoding.BinaryUnmarshaler, bool) {
	if ptr.Kind() == reflect.Ptr && ptr.IsNil() {
		p := reflect.New(ptr.Type().Elem())
		ptr.Set(p)
	}

	if obj, ok := ptr.Interface().(encoding.BinaryUnmarshaler); ok {
		return obj, true
	}

	pptr := ptr.Addr()

	if obj, ok := pptr.Interface().(encoding.BinaryUnmarshaler); ok {
		return obj, true
	}

	return nil, false
}

// RegisterName registers the named class with an instance of 'value'.  When the
// decoder finds a FREEZE tag with the given class, the binary data will be
// passed to value's UnmarshalBinary method.
func (d *Decoder) RegisterName(name string, value interface{}) {
	if d.umcache == nil {
		d.umcache = make(map[string]reflect.Type)
	}

	rv := reflect.ValueOf(value)
	if _, ok := value.(encoding.BinaryUnmarshaler); ok {
		d.umcache[name] = rv.Type()
		return
	}

	prv := rv.Addr()
	if _, ok := prv.Interface().(encoding.BinaryUnmarshaler); ok {
		d.umcache[name] = prv.Type()
		return
	}

	panic(fmt.Sprintf("unable to register type %s: not encoding.BinaryUnmarshaler", rv.Type()))
}

func (d *Decoder) getUnmarshalerType(name string) (reflect.Type, bool) {
	if d.umcache == nil {
		return nil, false
	}

	val, ok := d.umcache[name]
	return val, ok
}

func instantiateZero(typ reflect.Type) reflect.Value {
	if typ.Kind() == reflect.Ptr {
		return reflect.New(typ.Elem())
	}

	v := reflect.New(typ)
	return v.Addr()
}

// DecompressDocument changes the compression type of a Sereal document without performing a full re-serialization
//
// One use case is to use this before decoding, to avoid the implicit allocation.
func DecompressDocument(dst, b []byte) (r []byte, err error) {
	header, err := checkHeader(b)
	if err != nil {
		return nil, err
	}

	decomp, err := documentDecompressor(header.version, header.doctype)
	if err != nil {
		return nil, err
	}

	bodyStart := headerSize + header.suffixSize

	if bodyStart > len(b) || bodyStart < 0 {
		return nil, ErrCorrupt{errBadOffset}
	}

	if decomp != nil {
		var decompressInto []byte
		if dst != nil && len(dst) >= len(b) {
			decompressInto = dst[bodyStart:]
		} else {
			decompressInto = nil
		}

		decompBody, err := decomp.decompress(decompressInto, b[bodyStart:])
		if err != nil {
			return nil, err
		}

		if len(dst) > 0 && hasSameBuffer(decompBody, dst) {
			copy(dst[0:bodyStart], b[0:bodyStart])
			dst = dst[0 : bodyStart+len(decompBody)]
		} else {
			dst = make([]byte, 0, bodyStart+len(decompBody))
			dst = append(dst, b[0:bodyStart]...)
			dst = append(dst, decompBody...)
		}

		// set type to 0 (not compressed)
		dst[4] &= 0x0f
	} else {
		dst = append(dst[:0], b...)
	}

	return dst, nil
}

// hasSameBuffer returns true if the two slices share the same array (it only works for slices with capacity > 0)
func hasSameBuffer(a, b []byte) bool {
	aCap, bCap := cap(a), cap(b)
	aExt, bExt := a[:aCap], b[:bCap]

	return &aExt[aCap-1] == &bExt[bCap-1]
}
