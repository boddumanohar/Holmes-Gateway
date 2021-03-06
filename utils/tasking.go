package tasking

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"github.com/howeyc/fsnotify"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"time"
)

type Ticket struct {
	Expiration  time.Time
	Tasks       []Task
	SignerKeyId string
	Signature   []byte
}

// Tasks are encrypted with a symmetric key (EncryptedKey), which is
// encrypted with the asymmetric key in KeyFingerprint
type Encrypted struct {
	KeyFingerprint string
	EncryptedKey   []byte
	Encrypted      []byte
	IV             []byte
}

type Task struct {
	PrimaryURI   string              `json:"primaryURI"`
	SecondaryURI string              `json:"secondaryURI"`
	Filename     string              `json:"filename"`
	Tasks        map[string][]string `json:"tasks"`
	Tags         []string            `json:"tags"`
	Attempts     int                 `json:"attempts"`
	Source       string              `json:"source"`
	Download     bool                `json:"download"`
	Comment      string              `json:"comment"`
}

type Organization struct {
	Name    string
	Uri     string
	Sources []string
}

type User struct {
	Name         string `json:"name"`
	Id           int    `json:"id"`
	PasswordHash string `json:"pw"`
}

type ErrCode int

const (
	ERR_NONE                ErrCode = 1 + iota
	ERR_KEY_UNKNOWN                 = iota
	ERR_ENCRYPTION                  = iota
	ERR_TASK_INVALID                = iota
	ERR_NOT_ALLOWED                 = iota
	ERR_OTHER_UNRECOVERABLE         = iota
	ERR_OTHER_RECOVERABLE           = iota
)

type MyError struct {
	Error error
	Code  ErrCode
}

type TaskError struct {
	TaskStruct Task
	Error      MyError
}

type GatewayAnswer struct {
	Error     *MyError
	TskErrors []TaskError
}

func (me MyError) MarshalJSON() ([]byte, error) {
	return json.Marshal(
		struct {
			Error string
			Code  ErrCode
		}{
			Error: me.Error.Error(),
			Code:  me.Code,
		})
}

func (me *MyError) UnmarshalJSON(data []byte) error {
	var s struct {
		Error string
		Code  ErrCode
	}
	err := json.Unmarshal(data, &s)
	me.Error = errors.New(s.Error)
	me.Code = s.Code
	return err
}

func FailOnError(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %s", msg, err)
	}
}

func AesEncrypt(plaintext []byte, key []byte, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return []byte(""), err
	}
	mode := cipher.NewCBCEncrypter(block, iv)

	padLength := mode.BlockSize() - len(plaintext)%mode.BlockSize()
	ciphertext := make([]byte, len(plaintext))
	copy(ciphertext, plaintext)
	ciphertext = append(ciphertext, bytes.Repeat([]byte{byte(padLength)}, padLength)...)

	mode.CryptBlocks(ciphertext, ciphertext)
	return ciphertext, nil
}

func Sign(message []byte, key *rsa.PrivateKey) ([]byte, error) {
	hashed := sha256.Sum256(message)
	return rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
}

func Verify(signature []byte, message []byte, key *rsa.PublicKey) error {
	hashed := sha256.Sum256(message)
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, hashed[:], signature)
}

func VerifyTicket(ticket Ticket, key *rsa.PublicKey) error {
	sign := ticket.Signature
	ticket.Signature = nil
	msg, err := json.Marshal(ticket)
	if err != nil {
		return err
	}
	return Verify(sign, msg, key)
}

func AesDecrypt(ciphertext []byte, key []byte, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return []byte(""), err
	}
	mode := cipher.NewCBCDecrypter(block, iv)
	plaintext := make([]byte, len(ciphertext))
	mode.CryptBlocks(plaintext, ciphertext)
	if len(plaintext) == 0 {
		return []byte(""), errors.New("Empty plaintext")
	}
	padLength := int(plaintext[len(plaintext)-1])
	if padLength > len(plaintext) {
		return []byte(""), errors.New("Invalid padding size")
	}
	plaintext = plaintext[:len(plaintext)-padLength]
	return plaintext, nil
}

func RsaEncrypt(plaintext []byte, key *rsa.PublicKey) ([]byte, error) {
	label := []byte("")
	ciphertext, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, key, plaintext, label)
	return ciphertext, err
}

func RsaDecrypt(ciphertext []byte, key *rsa.PrivateKey) ([]byte, error) {
	label := []byte("")
	plaintext, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, key, ciphertext, label)
	return plaintext, err
}

func LoadPrivateKey(path string) (*rsa.PrivateKey, string, error) {
	log.Println(path)
	f, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, "Read", err
	}
	priv, rem := pem.Decode(f)
	if len(rem) != 0 || priv == nil {
		return nil, "Decode", errors.New("Key not in pem-format")
	}
	key, err := x509.ParsePKCS1PrivateKey(priv.Bytes)
	if err != nil {
		return nil, "Parse", err
	}

	// strip the path from its directory and ".priv"-extension
	path = filepath.Base(path)
	path = path[:len(path)-5]
	return (*rsa.PrivateKey)(key), path, nil
}

func LoadPublicKey(path string) (*rsa.PublicKey, string, error) {
	log.Println(path)
	f, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, "Read", err
	}
	pub, rem := pem.Decode(f)
	if len(rem) != 0 || pub == nil {
		return nil, "Decode", errors.New("Key not in pem-format")
	}
	key, err := x509.ParsePKIXPublicKey(pub.Bytes)
	if err != nil {
		return nil, "Parse", err
	}

	// strip the path from its directory and ".pub"-extension
	path = filepath.Base(path)
	path = path[:len(path)-4]
	return key.(*rsa.PublicKey), path, nil
}

func dirWatcherFunc(watcher *fsnotify.Watcher, ext string, onRemove func(string), onAdd func(string)) {
	for {
		select {
		case ev := <-watcher.Event:
			if filepath.Ext(ev.Name) != ext {
				continue
			}
			log.Println("event:", ev)
			if ev.IsCreate() {
				log.Println("New key", ev.Name)
				onAdd(ev.Name)
			} else if ev.IsDelete() || ev.IsRename() {
				// For renamed keys, there is a CREATE-event afterwards so it is just removed here
				log.Println("Removed key", ev.Name)
				name := filepath.Base(ev.Name)
				name = name[:len(name)-len(ext)]
				onRemove(name)
			} else if ev.IsModify() {
				log.Println("Modified key", ev.Name)
				onRemove(ev.Name)
				onAdd(ev.Name)
			}
			//log.Println(keys)

		case err := <-watcher.Error:
			log.Println("error:", err)
		}
	}
}

func DirWatcher(dir string, ext string, onRemove func(string), onAdd func(string)) {
	// Setup directory watcher
	watcher, err := fsnotify.NewWatcher()
	FailOnError(err, "Error setting up directory-watcher")

	// Process events
	go dirWatcherFunc(watcher, ext, onRemove, onAdd)
	err = watcher.Watch(dir)
	FailOnError(err, "Error setting up directory-watcher")
}

func keyWalkFn(ext string, onAdd func(string), path string, fi os.FileInfo, err error) error {
	if fi.IsDir() {
		return nil
	}
	if !(filepath.Ext(path) == ext) {
		return nil
	}
	onAdd(path)
	return nil
}

func LoadKeysAndWatch(dir string, ext string, onRemove func(string), onAdd func(string)) {
	err := filepath.Walk(dir,
		func(path string, fi os.FileInfo, err error) error {
			return keyWalkFn(ext, onAdd, path, fi, err)
		})
	FailOnError(err, "Error loading keys ")

	DirWatcher(dir, ext, onRemove, onAdd)
}
