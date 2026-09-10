package paho

// truncatePublishUserProperties rewrites one framed PUBLISH in place so that
// its properties section keeps only the first keep User Properties, every other
// property in its original order, and the payload; the properties length and
// the Remaining Length are re-encoded for the shorter body. bodyStart is where
// the variable header begins, after the fixed header byte and the Remaining
// Length digits. The result is never longer than the input, so it stays inside
// the guard's frame buffer, and every section lands at or below the offset it
// came from, so the in-place moves never overwrite bytes still to be read.
func truncatePublishUserProperties(
	packet []byte,
	bodyStart int,
	layout publishLayout,
	keep int,
) ([]byte, error) {
	body := packet[bodyStart:]
	head := body[:layout.propertiesLengthOffset]
	payload := body[layout.propertiesOffset+layout.propertiesLength:]
	properties, err := truncateRawUserProperties(
		body[layout.propertiesOffset:layout.propertiesOffset+layout.propertiesLength], keep,
	)
	if err != nil {
		return nil, err
	}

	var propertiesLength [4]byte
	propertiesLengthDigits := appendMQTTVBI(propertiesLength[:0], len(properties))
	var remainingLength [4]byte
	remainingLengthDigits := appendMQTTVBI(remainingLength[:0],
		len(head)+len(propertiesLengthDigits)+len(properties)+len(payload))

	// Sections are appended front to back over the original bytes. Each one
	// is moved to an offset no greater than its source and the sections keep
	// their order, so a move can only overwrite bytes already consumed.
	out := packet[:1]
	out = append(out, remainingLengthDigits...)
	out = append(out, head...)
	out = append(out, propertiesLengthDigits...)
	out = append(out, properties...)
	out = append(out, payload...)
	return out, nil
}

// truncateRawUserProperties compacts a raw PUBLISH properties section in place
// so that only the first keep User Properties remain; every other property is
// kept in order. It fails only on a section that validatePublish would have
// refused.
func truncateRawUserProperties(properties []byte, keep int) ([]byte, error) {
	out := properties[:0]
	kept := 0
	err := walkRawPublishProperties(properties, func(propertyID, start, end int) {
		if propertyID == mqttPropertyUserProperty {
			if kept == keep {
				return
			}
			kept++
		}
		out = append(out, properties[start:end]...)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// appendMQTTVBI appends value encoded as an MQTT variable byte integer.
func appendMQTTVBI(dst []byte, value int) []byte {
	for {
		digit := byte(value % 128)
		value /= 128
		if value > 0 {
			digit |= 0x80
		}
		dst = append(dst, digit)
		if value == 0 {
			return dst
		}
	}
}
