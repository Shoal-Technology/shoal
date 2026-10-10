package store

// CheckEncrypted returns an error if a record of one of kinds cannot be
// opened with key. A restore calls it before it writes anything, so a wrong
// secret.key is refused up front.
func CheckEncrypted(key []byte, recs []Record, kinds ...string) error {
	e, err := NewEncryptingStore(nil, key, kinds...)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if !e.encrypted(r.Kind) {
			continue
		}
		if _, err := e.open(r); err != nil {
			return err
		}
	}
	return nil
}
