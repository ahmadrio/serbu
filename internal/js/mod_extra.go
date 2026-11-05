package js

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"hash"
	"strings"

	"github.com/dop251/goja"
)

func (vu *VU) bytesOf(v goja.Value) []byte {
	switch x := v.Export().(type) {
	case string:
		return []byte(x)
	case goja.ArrayBuffer:
		return x.Bytes()
	case []byte:
		return x
	}
	return []byte(v.String())
}

func b64enc(name string) (*base64.Encoding, bool) {
	switch name {
	case "", "std":
		return base64.StdEncoding, true
	case "rawstd":
		return base64.RawStdEncoding, true
	case "url":
		return base64.URLEncoding, true
	case "rawurl":
		return base64.RawURLEncoding, true
	}
	return nil, false
}

// modEncoding = modul "k6/encoding": b64encode, b64decode.
func (vu *VU) modEncoding() goja.Value {
	o := vu.rt.NewObject()
	encName := func(v goja.Value) *base64.Encoding {
		name := ""
		if !isNil(v) {
			name = v.String()
		}
		e, ok := b64enc(name)
		if !ok {
			vu.throw("encoding base64 %q tidak dikenal (pilihan: std, rawstd, url, rawurl)", name)
		}
		return e
	}
	_ = o.Set("b64encode", func(c goja.FunctionCall) goja.Value {
		return vu.rt.ToValue(encName(c.Argument(1)).EncodeToString(vu.bytesOf(c.Argument(0))))
	})
	_ = o.Set("b64decode", func(c goja.FunctionCall) goja.Value {
		b, err := encName(c.Argument(1)).DecodeString(c.Argument(0).String())
		if err != nil {
			vu.throw("b64decode: input bukan base64 valid: %v", err)
		}
		if f := c.Argument(2); !isNil(f) && f.String() == "s" {
			return vu.rt.ToValue(string(b))
		}
		return vu.rt.ToValue(vu.rt.NewArrayBuffer(b))
	})
	return o
}

func hasher(name string) func() hash.Hash {
	switch strings.ToLower(name) {
	case "md5":
		return md5.New
	case "sha1":
		return sha1.New
	case "sha256":
		return sha256.New
	case "sha384":
		return sha512.New384
	case "sha512":
		return sha512.New
	case "sha512_224":
		return sha512.New512_224
	case "sha512_256":
		return sha512.New512_256
	}
	return nil
}

// modCrypto = modul "k6/crypto": md5, sha1, sha256, sha384, sha512, hmac, randomBytes.
func (vu *VU) modCrypto() goja.Value {
	o := vu.rt.NewObject()
	out := func(sum []byte, enc goja.Value) goja.Value {
		e := "binary"
		if !isNil(enc) {
			e = enc.String()
		}
		switch e {
		case "hex":
			return vu.rt.ToValue(hex.EncodeToString(sum))
		case "base64":
			return vu.rt.ToValue(base64.StdEncoding.EncodeToString(sum))
		case "base64url":
			return vu.rt.ToValue(base64.URLEncoding.EncodeToString(sum))
		case "base64rawurl":
			return vu.rt.ToValue(base64.RawURLEncoding.EncodeToString(sum))
		case "binary":
			return vu.rt.ToValue(vu.rt.NewArrayBuffer(sum))
		}
		vu.throw("output encoding %q tidak dikenal (pilihan: hex, base64, base64url, base64rawurl, binary)", e)
		return nil
	}
	for _, name := range []string{"md5", "sha1", "sha256", "sha384", "sha512", "sha512_224", "sha512_256"} {
		newH := hasher(name)
		_ = o.Set(name, func(c goja.FunctionCall) goja.Value {
			h := newH()
			h.Write(vu.bytesOf(c.Argument(0)))
			return out(h.Sum(nil), c.Argument(1))
		})
	}
	_ = o.Set("hmac", func(c goja.FunctionCall) goja.Value {
		newH := hasher(c.Argument(0).String())
		if newH == nil {
			vu.throw("algoritma hmac %q tidak dikenal", c.Argument(0).String())
		}
		m := hmac.New(newH, vu.bytesOf(c.Argument(1)))
		m.Write(vu.bytesOf(c.Argument(2)))
		return out(m.Sum(nil), c.Argument(3))
	})
	_ = o.Set("randomBytes", func(c goja.FunctionCall) goja.Value {
		n := c.Argument(0).ToInteger()
		if n < 1 || n > 1<<20 {
			vu.throw("randomBytes: jumlah byte harus 1..1048576")
		}
		b := make([]byte, n)
		_, _ = rand.Read(b)
		return vu.rt.ToValue(vu.rt.NewArrayBuffer(b))
	})
	return o
}

// modData = modul "k6/data": SharedArray. Data dibuat sekali lalu disalin ke tiap VU.
func (vu *VU) modData() goja.Value {
	o := vu.rt.NewObject()
	_ = o.Set("SharedArray", func(call goja.ConstructorCall) *goja.Object {
		if !vu.inInit {
			vu.throw("SharedArray harus dibuat di init context")
		}
		name := call.Argument(0).String()
		var js string
		if cached, ok := vu.sh.shared.Load(name); ok {
			js = cached.(string)
		} else {
			fn, ok := goja.AssertFunction(call.Argument(1))
			if !ok {
				vu.throw("SharedArray(%q, fn): argumen kedua harus function yang mengembalikan array", name)
			}
			v, err := fn(goja.Undefined())
			if err != nil {
				panic(err)
			}
			if obj, ok := v.(*goja.Object); !ok || obj.ClassName() != "Array" {
				vu.throw("SharedArray(%q): function harus mengembalikan array", name)
			}
			s, err := vu.stringify(v)
			if err != nil {
				panic(err)
			}
			actual, _ := vu.sh.shared.LoadOrStore(name, s)
			js = actual.(string)
		}
		v, err := vu.parseJSON(js)
		if err != nil {
			panic(err)
		}
		return v.ToObject(vu.rt)
	})
	return o
}

var utilsProgram = goja.MustCompile("serbu/utils", `(function () {
  var m = {};
  m.randomIntBetween = function (min, max) { return Math.floor(Math.random() * (max - min + 1) + min); };
  m.randomItem = function (arr) { return arr[Math.floor(Math.random() * arr.length)]; };
  m.randomString = function (length, charset) {
    charset = charset || 'abcdefghijklmnopqrstuvwxyz';
    var res = '';
    while (length--) res += charset[(Math.random() * charset.length) | 0];
    return res;
  };
  m.uuidv4 = function () {
    return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, function (c) {
      var r = (Math.random() * 16) | 0, v = c === 'x' ? r : (r & 0x3) | 0x8;
      return v.toString(16);
    });
  };
  m.findBetween = function (content, left, right, repeat) {
    var found = [], end = 0;
    while (true) {
      var start = content.indexOf(left, end);
      if (start === -1) break;
      start += left.length;
      end = content.indexOf(right, start);
      if (end === -1) break;
      var value = content.substring(start, end);
      if (!repeat) return value;
      found.push(value);
      end += right.length;
    }
    return found.length ? found : null;
  };
  return m;
})()`, true)

// modUtils = "serbu/utils" (juga dipakai untuk import jslib.k6.io/k6-utils).
func (vu *VU) modUtils() goja.Value {
	v, err := vu.rt.RunProgram(utilsProgram)
	if err != nil {
		panic(err)
	}
	return v
}

// modSummary = "serbu/summary" (juga untuk jslib.k6.io/k6-summary): textSummary(data, opts).
func (vu *VU) modSummary() goja.Value {
	o := vu.rt.NewObject()
	_ = o.Set("textSummary", func(c goja.FunctionCall) goja.Value {
		data, err := vu.stringify(c.Argument(0))
		if err != nil {
			panic(err)
		}
		color, indent := false, " "
		if opts := c.Argument(1); !isNil(opts) {
			oo := opts.ToObject(vu.rt)
			if v := oo.Get("enableColors"); !isNil(v) {
				color = v.ToBoolean()
			}
			if v := oo.Get("indent"); !isNil(v) {
				indent = v.String()
			}
		}
		s, err := vu.sh.Host.TextSummary(data, color, indent)
		if err != nil {
			vu.throw("%v", err)
		}
		return vu.rt.ToValue(s)
	})
	return o
}
