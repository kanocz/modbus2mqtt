package main

// set one bit in uin16
func SetBit(orig uint16, pos int) uint16 {
	orig |= (uint16(1) << pos)
	return orig
}

// clear one bit in uint16
func ClearBit(orig uint16, pos int) uint16 {
	mask := ^(uint16(1) << pos)
	orig &= mask
	return orig
}

// get one bit from uint16
func GetBit(orig uint16, pos int) bool {
	return (orig & (uint16(1) << pos)) > 0
}

// set X bits in uin16
func SetXBit(orig uint16, val uint16, pos int, bits int) uint16 {

	var mask uint16
	for i := pos; i < pos+bits; i++ {
		mask |= (uint16(1) << i)
	}

	// clear value only to needed bits
	val = (val << pos) & mask

	// clear orig value's bits
	orig &= ^mask

	return orig | val
}

// get X-bits from uint16 at specified position
func GetXBit(orig uint16, pos int, bits int) uint16 {

	var mask uint16
	for i := pos; i < pos+bits; i++ {
		mask |= (uint16(1) << i)
	}

	orig &= mask

	return orig >> pos
}
