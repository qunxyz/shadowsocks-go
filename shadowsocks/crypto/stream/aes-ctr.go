package stream

import (
	"crypto/cipher"
	"crypto/aes"
)

type AESCTR struct {
	Stream
}

func (this *AESCTR) new(key, iv []byte, doe DecOrEnc) (cipher.Stream, cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	return cipher.NewCTR(block, iv), nil, nil
}