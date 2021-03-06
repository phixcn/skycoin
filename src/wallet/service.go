package wallet

import (
	"fmt"
	"os"
	"sync"

	"github.com/skycoin/skycoin/src/cipher"
	"github.com/skycoin/skycoin/src/coin"
)

// BalanceGetter interface for getting the balance of given addresses
type BalanceGetter interface {
	GetBalanceOfAddrs(addrs []cipher.Address) ([]BalancePair, error)
}

// Service wallet service struct
type Service struct {
	sync.RWMutex
	wallets         Wallets
	firstAddrIDMap  map[string]string // Key: first address in wallet; Value: wallet id
	walletDirectory string
	cryptoType      CryptoType
	enableWalletAPI bool
	enableSeedAPI   bool
}

// Config wallet service config
type Config struct {
	WalletDir       string
	CryptoType      CryptoType
	EnableWalletAPI bool
	EnableSeedAPI   bool
}

// NewService new wallet service
func NewService(c Config) (*Service, error) {
	serv := &Service{
		firstAddrIDMap:  make(map[string]string),
		cryptoType:      c.CryptoType,
		enableWalletAPI: c.EnableWalletAPI,
		enableSeedAPI:   c.EnableSeedAPI,
	}

	if !serv.enableWalletAPI {
		return serv, nil
	}

	if err := os.MkdirAll(c.WalletDir, os.FileMode(0700)); err != nil {
		return nil, fmt.Errorf("failed to create wallet directory %s: %v", c.WalletDir, err)
	}

	serv.walletDirectory = c.WalletDir

	// Removes .wlt.bak files before loading wallets
	if err := removeBackupFiles(serv.walletDirectory); err != nil {
		return nil, fmt.Errorf("remove .wlt.bak files in %v failed: %v", serv.walletDirectory, err)
	}

	// Loads wallets
	w, err := LoadWallets(serv.walletDirectory)
	if err != nil {
		return nil, fmt.Errorf("failed to load all wallets: %v", err)
	}

	serv.wallets = serv.removeDup(w)

	return serv, nil
}

// CreateWallet creates a wallet with the given wallet file name and options.
// A address will be automatically generated by default.
func (serv *Service) CreateWallet(wltName string, options Options, bg BalanceGetter) (*Wallet, error) {
	serv.Lock()
	defer serv.Unlock()
	if !serv.enableWalletAPI {
		return nil, ErrWalletAPIDisabled
	}
	if wltName == "" {
		wltName = serv.generateUniqueWalletFilename()
	}

	return serv.loadWallet(wltName, options, bg)
}

// loadWallet loads wallet from seed and scan the first N addresses
func (serv *Service) loadWallet(wltName string, options Options, bg BalanceGetter) (*Wallet, error) {
	// service decides what crypto type the wallet should use.
	if options.Encrypt {
		options.CryptoType = serv.cryptoType
	}

	w, err := NewWalletScanAhead(wltName, options, bg)
	if err != nil {
		return nil, err
	}

	// Check for duplicate wallets by initial seed
	if _, ok := serv.firstAddrIDMap[w.Entries[0].Address.String()]; ok {
		return nil, ErrSeedUsed
	}

	if err := serv.wallets.add(w); err != nil {
		return nil, err
	}

	if err := w.Save(serv.walletDirectory); err != nil {
		// If save fails, remove the added wallet
		serv.wallets.remove(w.Filename())
		return nil, err
	}

	serv.firstAddrIDMap[w.Entries[0].Address.String()] = w.Filename()

	return w.clone(), nil
}

func (serv *Service) generateUniqueWalletFilename() string {
	wltName := newWalletFilename()
	for {
		if _, ok := serv.wallets.get(wltName); !ok {
			break
		}
		wltName = newWalletFilename()
	}

	return wltName
}

// EncryptWallet encrypts wallet with password
func (serv *Service) EncryptWallet(wltID string, password []byte) (*Wallet, error) {
	serv.Lock()
	defer serv.Unlock()
	if !serv.enableWalletAPI {
		return nil, ErrWalletAPIDisabled
	}

	w, err := serv.getWallet(wltID)
	if err != nil {
		return nil, err
	}

	if w.IsEncrypted() {
		return nil, ErrWalletEncrypted
	}

	if err := w.Lock(password, serv.cryptoType); err != nil {
		return nil, err
	}

	// Save to disk first
	if err := w.Save(serv.walletDirectory); err != nil {
		return nil, err
	}

	// Sets the encrypted wallet
	serv.wallets.set(w)
	return w, nil
}

// DecryptWallet decrypts wallet with password
func (serv *Service) DecryptWallet(wltID string, password []byte) (*Wallet, error) {
	serv.Lock()
	defer serv.Unlock()
	if !serv.enableWalletAPI {
		return nil, ErrWalletAPIDisabled
	}

	w, err := serv.getWallet(wltID)
	if err != nil {
		return nil, err
	}

	// Returns error if wallet is not encrypted
	if !w.IsEncrypted() {
		return nil, ErrWalletNotEncrypted
	}

	// Unlocks the wallet
	unlockWlt, err := w.Unlock(password)
	if err != nil {
		return nil, err
	}

	// Updates the wallet file
	if err := unlockWlt.Save(serv.walletDirectory); err != nil {
		return nil, err
	}

	// Sets the decrypted wallet in memory
	serv.wallets.set(unlockWlt)
	return unlockWlt, nil
}

// NewAddresses generate address entries in given wallet,
// return nil if wallet does not exist.
// Set password as nil if the wallet is not encrypted, otherwise the password must be provided.
func (serv *Service) NewAddresses(wltID string, password []byte, num uint64) ([]cipher.Address, error) {
	serv.Lock()
	defer serv.Unlock()

	if !serv.enableWalletAPI {
		return nil, ErrWalletAPIDisabled
	}

	w, err := serv.getWallet(wltID)
	if err != nil {
		return nil, err
	}

	var addrs []cipher.Address
	f := func(wlt *Wallet) error {
		var err error
		addrs, err = wlt.GenerateAddresses(num)
		return err
	}

	if w.IsEncrypted() {
		if err := w.GuardUpdate(password, f); err != nil {
			return nil, err
		}
	} else {
		if err := f(w); err != nil {
			return nil, err
		}
	}

	// Set the updated wallet back
	serv.wallets.set(w)

	if err := w.Save(serv.walletDirectory); err != nil {
		return []cipher.Address{}, err
	}

	return addrs, nil
}

// GetAddresses returns all addresses in given wallet
func (serv *Service) GetAddresses(wltID string) ([]cipher.Address, error) {
	serv.RLock()
	defer serv.RUnlock()
	if !serv.enableWalletAPI {
		return nil, ErrWalletAPIDisabled
	}

	w, err := serv.getWallet(wltID)
	if err != nil {
		return nil, err
	}

	return w.GetAddresses(), nil
}

// GetWallet returns wallet by id
func (serv *Service) GetWallet(wltID string) (*Wallet, error) {
	serv.RLock()
	defer serv.RUnlock()
	if !serv.enableWalletAPI {
		return nil, ErrWalletAPIDisabled
	}

	return serv.getWallet(wltID)
}

// returns the clone of the wallet of given id
func (serv *Service) getWallet(wltID string) (*Wallet, error) {
	w, ok := serv.wallets.get(wltID)
	if !ok {
		return nil, ErrWalletNotExist
	}
	return w.clone(), nil
}

// GetWallets returns all wallet clones
func (serv *Service) GetWallets() (Wallets, error) {
	serv.RLock()
	defer serv.RUnlock()
	if !serv.enableWalletAPI {
		return nil, ErrWalletAPIDisabled
	}

	wlts := make(Wallets, len(serv.wallets))
	for k, w := range serv.wallets {
		wlts[k] = w.clone()
	}
	return wlts, nil
}

// ReloadWallets reload wallets
func (serv *Service) ReloadWallets() error {
	serv.Lock()
	defer serv.Unlock()
	if !serv.enableWalletAPI {
		return ErrWalletAPIDisabled
	}
	wallets, err := LoadWallets(serv.walletDirectory)
	if err != nil {
		return err
	}

	serv.firstAddrIDMap = make(map[string]string)
	serv.wallets = serv.removeDup(wallets)
	return nil
}

// CreateAndSignTransaction creates and signs a transaction from wallet.
// Set the password as nil if the wallet is not encrypted, otherwise the password must be provided
func (serv *Service) CreateAndSignTransaction(wltID string, password []byte, auxs coin.AddressUxOuts, headTime, coins uint64, dest cipher.Address) (*coin.Transaction, error) {
	serv.RLock()
	defer serv.RUnlock()
	if !serv.enableWalletAPI {
		return nil, ErrWalletAPIDisabled
	}

	w, err := serv.getWallet(wltID)
	if err != nil {
		return nil, err
	}

	var tx *coin.Transaction
	f := func(wlt *Wallet) error {
		var err error
		tx, err = wlt.CreateAndSignTransaction(auxs, headTime, coins, dest)
		return err
	}

	if w.IsEncrypted() {
		if err := w.GuardView(password, f); err != nil {
			return nil, err
		}
	} else {
		if err := f(w); err != nil {
			return nil, err
		}
	}
	return tx, nil
}

// CreateAndSignTransactionAdvanced creates and signs a transaction based upon CreateTransactionParams.
// Set the password as nil if the wallet is not encrypted, otherwise the password must be provided
func (serv *Service) CreateAndSignTransactionAdvanced(params CreateTransactionParams, auxs coin.AddressUxOuts, headTime uint64) (*coin.Transaction, []UxBalance, error) {
	serv.RLock()
	defer serv.RUnlock()

	if !serv.enableWalletAPI {
		return nil, nil, ErrWalletAPIDisabled
	}

	if err := params.Validate(); err != nil {
		return nil, nil, err
	}

	w, err := serv.getWallet(params.Wallet.ID)
	if err != nil {
		return nil, nil, err
	}

	// Check if the wallet needs a password
	if w.IsEncrypted() {
		if len(params.Wallet.Password) == 0 {
			return nil, nil, ErrMissingPassword
		}
	} else {
		if len(params.Wallet.Password) != 0 {
			return nil, nil, ErrWalletNotEncrypted
		}
	}

	var tx *coin.Transaction
	var inputs []UxBalance
	if w.IsEncrypted() {
		err = w.GuardView(params.Wallet.Password, func(wlt *Wallet) error {
			var err error
			tx, inputs, err = wlt.CreateAndSignTransactionAdvanced(params, auxs, headTime)
			return err
		})
	} else {
		tx, inputs, err = w.CreateAndSignTransactionAdvanced(params, auxs, headTime)
	}
	if err != nil {
		return nil, nil, err
	}

	return tx, inputs, nil
}

// UpdateWalletLabel updates the wallet label
func (serv *Service) UpdateWalletLabel(wltID, label string) error {
	serv.Lock()
	defer serv.Unlock()
	if !serv.enableWalletAPI {
		return ErrWalletAPIDisabled
	}

	var wlt *Wallet
	if err := serv.wallets.update(wltID, func(w *Wallet) error {
		w.setLabel(label)
		wlt = w
		return nil
	}); err != nil {
		return err
	}

	return wlt.Save(serv.walletDirectory)
}

// Remove removes wallet of given wallet id from the service
func (serv *Service) Remove(wltID string) error {
	serv.Lock()
	defer serv.Unlock()
	if !serv.enableWalletAPI {
		return ErrWalletAPIDisabled
	}

	serv.wallets.remove(wltID)
	return nil
}

func (serv *Service) removeDup(wlts Wallets) Wallets {
	var rmWltIDS []string
	// remove dup wallets
	for wltID, wlt := range wlts {
		if len(wlt.Entries) == 0 {
			// empty wallet
			rmWltIDS = append(rmWltIDS, wltID)
			continue
		}

		addr := wlt.Entries[0].Address.String()
		id, ok := serv.firstAddrIDMap[addr]

		if ok {
			// check whose entries number is bigger
			pw, _ := wlts.get(id)

			if len(pw.Entries) >= len(wlt.Entries) {
				rmWltIDS = append(rmWltIDS, wltID)
				continue
			}

			// replace the old wallet with the new one
			// records the wallet id that need to remove
			rmWltIDS = append(rmWltIDS, id)
			// update wallet id
			serv.firstAddrIDMap[addr] = wltID
			continue
		}

		serv.firstAddrIDMap[addr] = wltID
	}

	// remove the duplicate and empty wallet
	for _, id := range rmWltIDS {
		wlts.remove(id)
	}

	return wlts
}

// GetWalletSeed returns seed of encrypted wallet of given wallet id
// Returns ErrWalletNotEncrypted if it's not encrypted
func (serv *Service) GetWalletSeed(wltID string, password []byte) (string, error) {
	serv.RLock()
	defer serv.RUnlock()
	if !serv.enableWalletAPI {
		return "", ErrWalletAPIDisabled
	}

	if !serv.enableSeedAPI {
		return "", ErrSeedAPIDisabled
	}

	w, err := serv.getWallet(wltID)
	if err != nil {
		return "", err
	}

	if !w.IsEncrypted() {
		return "", ErrWalletNotEncrypted
	}

	var seed string
	if err := w.GuardView(password, func(wlt *Wallet) error {
		seed = wlt.seed()
		return nil
	}); err != nil {
		return "", err
	}

	return seed, nil
}
