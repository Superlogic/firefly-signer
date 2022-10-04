// Copyright © 2022 Kaleido, Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fswallet

import (
	"context"
	"encoding/json"
	"io/fs"
	"io/ioutil"
	"path"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/hyperledger/firefly-common/pkg/fftypes"
	"github.com/hyperledger/firefly-common/pkg/i18n"
	"github.com/hyperledger/firefly-common/pkg/log"
	"github.com/hyperledger/firefly-signer/internal/signermsgs"
	"github.com/hyperledger/firefly-signer/pkg/ethsigner"
	"github.com/hyperledger/firefly-signer/pkg/ethtypes"
	"github.com/hyperledger/firefly-signer/pkg/keystorev3"
	"github.com/hyperledger/firefly-signer/pkg/secp256k1"
	"github.com/karlseguin/ccache"
	"github.com/pelletier/go-toml"
	"gopkg.in/yaml.v2"
)

// Wallet is a directory containing a set of KeystoreV3 files, conforming
// to the ethsigner.Wallet interface and providing notifications when new
// keys are added to the wallet (via FS listener).
type Wallet interface {
	ethsigner.Wallet
	AddListener(listener chan<- ethtypes.Address0xHex)
}

func NewFilesystemWallet(ctx context.Context, conf *Config, initialListeners ...chan<- ethtypes.Address0xHex) (ww Wallet, err error) {
	w := &fsWallet{
		conf:             *conf,
		listeners:        initialListeners,
		addressToFileMap: make(map[ethtypes.Address0xHex]string),
	}
	w.signerCache = ccache.New(
		// We use a LRU cache with a size-aware max
		ccache.Configure().
			MaxSize(fftypes.ParseToByteSize(conf.SignerCacheSize)),
	)
	w.metadataKeyFileProperty, err = goTemplateFromConfig(ctx, ConfigMetadataKeyFileProperty, conf.Metadata.KeyFileProperty)
	if err != nil {
		return nil, err
	}
	w.metadataPasswordFileProperty, err = goTemplateFromConfig(ctx, ConfigMetadataPasswordFileProperty, conf.Metadata.PasswordFileProperty)
	if err != nil {
		return nil, err
	}
	if conf.Filenames.PrimaryMatchRegex != "" {
		if w.primaryMatchRegex, err = regexp.Compile(conf.Filenames.PrimaryMatchRegex); err != nil {
			return nil, i18n.NewError(ctx, signermsgs.MsgBadRegularExpression, ConfigFilenamesPrimaryMatchRegex, err)
		}
		if len(w.primaryMatchRegex.SubexpNames()) < 2 {
			return nil, i18n.NewError(ctx, signermsgs.MsgMissingRegexpCaptureGroup, w.primaryMatchRegex.String())
		}
	}
	return w, nil
}

func goTemplateFromConfig(ctx context.Context, name string, templateStr string) (*template.Template, error) {
	if templateStr == "" {
		return nil, nil
	}
	t, err := template.New(name).Parse(templateStr)
	if err != nil {
		return nil, i18n.NewError(ctx, signermsgs.MsgBadGoTemplate, name)
	}
	return t, nil
}

type fsWallet struct {
	conf                         Config
	signerCache                  *ccache.Cache
	signerCacheTTL               time.Duration
	metadataKeyFileProperty      *template.Template
	metadataPasswordFileProperty *template.Template
	primaryMatchRegex            *regexp.Regexp

	mux               sync.Mutex
	addressToFileMap  map[ethtypes.Address0xHex]string
	listeners         []chan<- ethtypes.Address0xHex
	fsListenerCancel  context.CancelFunc
	fsListenerStarted chan error
	fsListenerDone    chan struct{}
}

func (w *fsWallet) Sign(ctx context.Context, txn *ethsigner.Transaction, chainID int64) ([]byte, error) {
	keypair, err := w.getSignerForAccount(ctx, txn.From)
	if err != nil {
		return nil, err
	}
	return txn.Sign(keypair, chainID)
}

func (w *fsWallet) Initialize(ctx context.Context) error {
	// Run a get accounts pass, to check all is ok
	lCtx, lCancel := context.WithCancel(log.WithLogField(ctx, "fswallet", w.conf.Path))
	w.fsListenerCancel = lCancel
	w.fsListenerStarted = make(chan error)
	w.fsListenerDone = make(chan struct{})
	go w.fsListener(lCtx)
	// Make sure listener is listening for changes, before doing the scan
	if err := <-w.fsListenerStarted; err != nil {
		return err
	}
	// Do an initial full scan before returning
	return w.Refresh(ctx)
}

func (w *fsWallet) AddListener(listener chan<- ethtypes.Address0xHex) {
	w.mux.Lock()
	defer w.mux.Unlock()
	w.listeners = append(w.listeners, listener)
}

// GetAccounts returns the currently cached list of known addresses
func (w *fsWallet) GetAccounts(ctx context.Context) ([]*ethtypes.Address0xHex, error) {
	w.mux.Lock()
	defer w.mux.Unlock()
	accounts := make([]*ethtypes.Address0xHex, 0, len(w.addressToFileMap))
	for addr := range w.addressToFileMap {
		a := addr
		accounts = append(accounts, &a)
	}
	return accounts, nil
}

func (w *fsWallet) matchFilename(ctx context.Context, f fs.FileInfo) *ethtypes.Address0xHex {
	if f.IsDir() {
		log.L(ctx).Tracef("Ignoring '%s/%s: directory", w.conf.Path, f.Name())
		return nil
	}
	if w.primaryMatchRegex != nil {
		match := w.primaryMatchRegex.FindStringSubmatch(f.Name())
		if match == nil {
			log.L(ctx).Tracef("Ignoring '%s/%s': does not match regexp", w.conf.Path, f.Name())
			return nil
		}
		addr, err := ethtypes.NewAddress(match[1]) // safe due to SubexpNames() length check
		if err != nil {
			log.L(ctx).Warnf("Ignoring '%s/%s': invalid address '%s': %s", w.conf.Path, f.Name(), match[1], err)
			return nil
		}
		return addr
	}
	if !strings.HasSuffix(f.Name(), w.conf.Filenames.PrimaryExt) {
		log.L(ctx).Tracef("Ignoring '%s/%s: does not match extension '%s'", w.conf.Path, f.Name(), w.conf.Filenames.PrimaryExt)
	}
	addrString := strings.TrimSuffix(f.Name(), w.conf.Filenames.PrimaryExt)
	addr, err := ethtypes.NewAddress(addrString)
	if err != nil {
		log.L(ctx).Warnf("Ignoring '%s/%s': invalid address '%s': %s", w.conf.Path, f.Name(), addrString, err)
		return nil
	}
	return addr
}

func (w *fsWallet) Refresh(ctx context.Context) error {
	files, err := ioutil.ReadDir(w.conf.Path)
	if err != nil {
		return i18n.WrapError(ctx, err, signermsgs.MsgReadDirFile)
	}
	return w.notifyNewFiles(ctx, files...)
}

func (w *fsWallet) notifyNewFiles(ctx context.Context, files ...fs.FileInfo) error {
	// Lock now we have the list
	w.mux.Lock()
	defer w.mux.Unlock()
	newAddresses := make([]*ethtypes.Address0xHex, 0)
	for _, f := range files {
		addr := w.matchFilename(ctx, f)
		if addr != nil {
			if existing := w.addressToFileMap[*addr]; existing != f.Name() {
				w.addressToFileMap[*addr] = f.Name()
				newAddresses = append(newAddresses, addr)
			}
		}
	}
	listeners := make([]chan<- ethtypes.Address0xHex, len(w.listeners))
	copy(listeners, w.listeners)
	log.L(ctx).Infof("Detected %d files. Found %d addresses", len(files), len(newAddresses))
	// Avoid holding the lock while calling the listeners, by using a go-routine
	go func() {
		for _, l := range w.listeners {
			for _, addr := range newAddresses {
				l <- *addr
			}
		}
	}()
	return nil
}

func (w *fsWallet) Close() error {
	return nil
}

func (w *fsWallet) getSignerForAccount(ctx context.Context, rawAddrJSON json.RawMessage) (*secp256k1.KeyPair, error) {

	// We require an ethereum address in the "from" field
	var from ethtypes.Address0xHex
	err := json.Unmarshal(rawAddrJSON, &from)
	if err != nil {
		return nil, err
	}

	addrString := from.String()
	cached := w.signerCache.Get(from.String())
	if cached != nil {
		cached.Extend(w.signerCacheTTL)
		return cached.Value().(*secp256k1.KeyPair), nil
	}

	w.mux.Lock()
	primaryFilename, ok := w.addressToFileMap[from]
	w.mux.Unlock()
	if !ok {
		return nil, i18n.NewError(ctx, signermsgs.MsgWalletNotAvailable, from)
	}

	keypair, err := w.loadKey(ctx, from, path.Join(w.conf.Path, primaryFilename))
	if err != nil {
		return nil, err
	}

	if keypair.Address != from {
		return nil, i18n.NewError(ctx, signermsgs.MsgAddressMismatch, keypair.Address, from)
	}

	w.signerCache.Set(addrString, keypair, w.signerCacheTTL)
	return keypair, err

}

func (w *fsWallet) loadKey(ctx context.Context, addr ethtypes.Address0xHex, primaryFilename string) (*secp256k1.KeyPair, error) {

	b, err := ioutil.ReadFile(primaryFilename)
	if err != nil {
		log.L(ctx).Errorf("Failed to read '%s': %s", primaryFilename, err)
		return nil, i18n.NewError(ctx, signermsgs.MsgWalletFailed, addr)
	}

	keyFilename, passwordFilename, err := w.getKeyAndPasswordFiles(ctx, addr, primaryFilename, b)
	if err != nil {
		return nil, err
	}
	log.L(ctx).Debugf("Reading keyfile=%s passwordfile=%s", keyFilename, passwordFilename)

	if keyFilename != primaryFilename {
		b, err = ioutil.ReadFile(keyFilename)
		if err != nil {
			log.L(ctx).Errorf("Failed to read '%s' (keyfile): %s", keyFilename, err)
			return nil, i18n.NewError(ctx, signermsgs.MsgWalletFailed, addr)
		}
	}

	var password []byte
	if passwordFilename != "" {
		password, err = ioutil.ReadFile(passwordFilename)
		if err != nil {
			log.L(ctx).Debugf("Failed to read '%s' (password file): %s", passwordFilename, err)
		}
	}

	// fall back to default password file
	if password == nil {
		if w.conf.DefaultPasswordFile == "" {
			log.L(ctx).Errorf("No password file available for address, and no default password file: %s", addr)
			return nil, i18n.NewError(ctx, signermsgs.MsgWalletFailed, addr)
		}
		password, err = ioutil.ReadFile(w.conf.DefaultPasswordFile)
		if err != nil {
			log.L(ctx).Errorf("Failed to read '%s' (default password file): %s", w.conf.DefaultPasswordFile, err)
			return nil, i18n.NewError(ctx, signermsgs.MsgWalletFailed, addr)
		}

	}

	// Ok - now we have what we need to open up the keyfile
	kv3, err := keystorev3.ReadWalletFile(b, password)
	if err != nil {
		log.L(ctx).Errorf("Failed to read '%s' (bad keystorev3 file): %s", w.conf.DefaultPasswordFile, err)
		return nil, i18n.NewError(ctx, signermsgs.MsgWalletFailed, addr)
	}
	return kv3.KeyPair(), nil

}

func (w *fsWallet) getKeyAndPasswordFiles(ctx context.Context, addr ethtypes.Address0xHex, primaryFilename string, primaryFile []byte) (kf string, pf string, err error) {
	if strings.ToLower(w.conf.Metadata.Format) == "auto" {
		w.conf.Metadata.Format = strings.TrimPrefix(w.conf.Filenames.PrimaryExt, ".")
	}

	var metadata map[string]interface{}
	switch w.conf.Metadata.Format {
	case "toml", "tml":
		err = toml.Unmarshal(primaryFile, &metadata)
	case "json":
		err = json.Unmarshal(primaryFile, &metadata)
	case "yaml", "yml":
		err = yaml.Unmarshal(primaryFile, &metadata)
	default:
		// No separate metadata file - we just use the default password file extension instead
		passwordFilename := ""
		if w.conf.Filenames.PasswordExt != "" {
			extToRemove := w.conf.Filenames.PrimaryExt
			if extToRemove == "" {
				// We use the first index - so remove '.key.json' for example
				filename := path.Base(primaryFilename)
				firstIndex := strings.Index(filename, ".")
				if firstIndex > 0 {
					extToRemove = filename[firstIndex:]
				}
			}
			passwordFilename = strings.TrimSuffix(primaryFilename, extToRemove) + w.conf.Filenames.PasswordExt
		}
		return primaryFilename, passwordFilename, nil
	}
	if err != nil {
		log.L(ctx).Errorf("Failed to parse '%s' as %s: %s", primaryFilename, w.conf.Metadata.Format, err)
		return "", "", i18n.NewError(ctx, signermsgs.MsgWalletFailed, addr)
	}

	kf, err = w.goTemplateToString(ctx, primaryFilename, metadata, w.metadataKeyFileProperty)
	if err == nil {
		pf, err = w.goTemplateToString(ctx, primaryFilename, metadata, w.metadataPasswordFileProperty)
	}
	if err != nil || kf == "" {
		return "", "", i18n.NewError(ctx, signermsgs.MsgWalletFailed, addr)
	}
	return kf, pf, nil
}

func (w *fsWallet) goTemplateToString(ctx context.Context, filename string, data map[string]interface{}, t *template.Template) (string, error) {
	if t == nil {
		return "", nil
	}
	buff := new(strings.Builder)
	err := t.Execute(buff, data)
	val := buff.String()
	if strings.Contains(val, "<no value>") || err != nil {
		log.L(ctx).Errorf("Failed to execute go template against metadata file %s: err=%v", filename, err)
		return "", nil
	}
	return val, err
}
