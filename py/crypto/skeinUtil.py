import random
import struct

from skein import skein256
from core.util import splitFields, splitField, xor, encode

SEED_SIZE=16
BLOCK_SIZE=32

def hash(data, pers=None):
  if pers:
    return skein256(data, pers=pers, digest_bits=256).digest()
  else:
    return skein256(data, digest_bits=256).digest()


def pbkdf(pb, salt, i, pers=None, digest_bits=256):
  data=(pb.encode('ascii')+salt)*i
  if pers:
    return skein256(data, pers=pers, digest_bits=digest_bits).digest()
  else:
    return skein256(data, digest_bits=digest_bits).digest()

class SkeinPRNG:
  def __init__(self, seed=None, pers=None):
    if seed:
      self.seed=seed
    else:
      self.seed=self.generateSeed()
    self.pers=pers

  def generateSeed(self):
    return bytes(random.randint(0, 255) for _ in range(SEED_SIZE))

  def reseed(self, seed):
    if self.pers:
      self.seed=skein256(self.seed+seed, pers=self.pers, digest_bits=SEED_SIZE*8).digest()
    else:
      self.seed=skein256(self.seed+seed, digest_bits=SEED_SIZE*8).digest()

  def getBytes(self, n):
    if self.pers:
      result=skein256(self.seed, pers=self.pers, digest_bits=(SEED_SIZE+n)*8).digest()
    else:
      result=skein256(self.seed, digest_bits=(SEED_SIZE+n)*8).digest()
    self.seed, r=splitFields(result, [SEED_SIZE, n])
    return r

  def getInt(self, max=None):
    bs=self.getBytes(4)
    i=struct.unpack('I', bs)[0]
    if max:
      return i%max
    else:
      return i

def encrypt(k, iv, data):
  cipher=SkeinCipherOFB(k, iv)
  return cipher.encrypt(data)

def decrypt(k, iv, data):
  cipher=SkeinCipherOFB(k, iv)
  return cipher.decrypt(data)

class SkeinCipherOFB:
  def __init__(self, key, iv, pers=None):
    self.key=key
    self.iv=iv
    self.entropy=b''
    self.pers=pers

  def getBytes(self, n):
    while len(self.entropy)<n:
      if self.pers:
        result=skein256(nonce=self.iv, mac=self.key, pers=self.pers, digest_bits=(BLOCK_SIZE)*8).digest()
      else:
        result=skein256(nonce=self.iv, mac=self.key, digest_bits=(BLOCK_SIZE)*8).digest()
      self.entropy=self.entropy+result
      self.iv=result
    b, self.entropy=splitField(self.entropy, n)
    return b

  def encrypt(self, data):
    l=len(data)
    entropy=self.getBytes(l)
    return xor(data, entropy)

  def decrypt(self, data):
    return self.encrypt(data)