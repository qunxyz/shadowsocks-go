package shadowsocks

import (
	"io"
	"math"
	"bytes"
)

type ConnAead struct {
	dataBuffer *bytes.Buffer
	reader io.Reader

	CipherInst *CipherAead
}

func (this *ConnAead) getPayloadSizeMask() int {
	return 0x3FFF // 16*1024 - 1
}

//func (this *ConnAead) getBuffer() *LeakyBufType {
//	return NewLeakyBuf(maxNBuf, this.getPayloadSizeMask())
//}

func (this *ConnAead) Init(r io.Reader, cipher *Cipher) {
	this.CipherInst = cipher.Inst.(*CipherAead)
	this.dataBuffer = bytes.NewBuffer(nil)
	this.reader = r
}

func (this *ConnAead) initEncrypt(r io.Reader, cipher *Cipher) (err error) {
	if this.CipherInst != nil && this.CipherInst.Enc != nil {
		return
	}
	this.Init(r, cipher)

	err = this.CipherInst.Init(nil, Encrypt)
	if err != nil {
		return
	}

	_, err = this.dataBuffer.Write(this.CipherInst.iv)
	if err != nil {
		Logger.Fields(LogFields{
			"err": err,
		}).Warn("write iv to connection error")
		return
	}

	return
}

func (this *ConnAead) initDecrypt(r io.Reader, cipher *Cipher) (err error) {
	if this.CipherInst != nil && this.CipherInst.Dec != nil {
		return
	}
	this.Init(r, cipher)

	var iv []byte
	iv, err = this.getIV()
	if err != nil {
		Logger.Fields(LogFields{
			"err": err,
		}).Warn("get iv from connection error")
		return
	}

	err = this.CipherInst.Init(iv, Decrypt)
	if err != nil {
		Logger.Fields(LogFields{
			"iv": iv,
			"this.cipher.iv": this.CipherInst.iv,
			"err": err,
		}).Warn("decrypt init error")
		return
	}
	return
}

func (this *ConnAead) getIV() (iv []byte, err error) {
	iv = make([]byte, this.CipherInst.IVSize())
	if _, err = io.ReadFull(this.reader, iv); err != nil {
		Logger.Fields(LogFields{
			"iv": iv,
			"err": err,
		}).Warn("shadowsocks: read iv from connect error")
		return
	}

	return
}

func (this *ConnAead) Pack(packet_data []byte) (err error) {
	packet_len := len(packet_data)
	chunk_num := math.Ceil(float64(packet_len)/float64(this.getPayloadSizeMask()))

	for chunk_counter := chunk_num; chunk_counter > 0;  {
		payload_len := packet_len
		if payload_len > this.getPayloadSizeMask() {
			payload_len = this.getPayloadSizeMask()
		}

		packet_buf := make([]byte, 2+this.CipherInst.Enc.Overhead()+payload_len+this.CipherInst.Enc.Overhead())
		payload_buf := packet_buf[2+this.CipherInst.Enc.Overhead() : 2+this.CipherInst.Enc.Overhead()+payload_len+this.CipherInst.Enc.Overhead()]

		// get header
		packet_buf[0], packet_buf[1] = byte(payload_len>>8), byte(payload_len)

		// pack header
		err = this.CipherInst.Encrypt(packet_buf, packet_buf[:2])
		if err != nil {
			Logger.Fields(LogFields{
				"header": packet_buf[:2],
				"this.cipher.iv": this.CipherInst.iv,
				"nonce": this.CipherInst.nonce,
				"err": err,
			}).Warn("encrypt header error")
			break
		}

		// get payload
		payload := packet_data[:payload_len]

		// pack payload
		err = this.CipherInst.Encrypt(payload_buf, payload)
		if err != nil {
			Logger.Fields(LogFields{
				"payload": payload_buf,
				"this.cipher.iv": this.CipherInst.iv,
				"nonce": this.CipherInst.nonce,
				"err": err,
			}).Warn("encrypt payload error")
			break
		}

		_, err = this.dataBuffer.Write(packet_buf)
		if err != nil {
			Logger.Fields(LogFields{
				"data": packet_buf,
				"err": err,
			}).Warn("write data to buffer error")
			break
		}
		chunk_counter--
		packet_len -= payload_len
	}

	return
}

func (this *ConnAead) UnPack() (err error) {
	/// read header
	header := make([]byte, 2+this.CipherInst.Dec.Overhead())

	if _, err = io.ReadFull(this.reader, header); err != nil {
		Logger.Fields(LogFields{
			"err": err,
		}).Warn("read data error")
		return
	}

	/// unpack header
	err = this.CipherInst.Decrypt(header, header) // decrypt packet data
	if err != nil {
		Logger.Fields(LogFields{
			"header": header,
			"this.cipher.iv": this.CipherInst.iv,
			"nonce": this.CipherInst.nonce,
			"err": err,
		}).Warn("decrypt header error")
		return
	}

	/// get payload size
	payload_size := int(header[0])<<8 + int(header[1]) & this.getPayloadSizeMask()

	/// read payload encrypted data
	payload := make([]byte, payload_size+this.CipherInst.Dec.Overhead())
	if _, err = io.ReadFull(this.reader, payload); err != nil {
		Logger.Fields(LogFields{
			"err": err,
		}).Warn("read data error")
		return
	}

	/// unpack payload
	err = this.CipherInst.Decrypt(payload, payload) // decrypt packet data
	if err != nil {
		Logger.Fields(LogFields{
			//"data_str": string(data),
			"payload_len": len(payload),
			"payload": payload,
			"this.cipher.iv": this.CipherInst.iv,
			"nonce": this.CipherInst.nonce,
			"err": err,
		}).Warn("decrypt payload error")
		return
	}
	payload = payload[:payload_size]
	//this.CipherInst.SetNonce(true)

	_, err = this.dataBuffer.Write(payload)
	if err != nil {
		Logger.Fields(LogFields{
			"data": payload,
			"data_str": string(payload),
			"err": err,
		}).Warn("write data to connection error")
		return
	}
	return
}