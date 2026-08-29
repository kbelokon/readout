// Ensure a byte buffer can retain its existing prefix and accept one append.
// The caller guarantees that the current capacity is within hardLimit, that
// retainedBytes is within the current buffer, and that retainedBytes plus
// appendedBytes does not exceed hardLimit. When growth is needed, capacity
// grows geometrically (or by one larger append), is clamped to the hard limit,
// and only the retained prefix is copied.
export function ensureBoundedByteBufferCapacity(
    buffer: Uint8Array<ArrayBuffer>,
    retainedBytes: number,
    appendedBytes: number,
    hardLimit: number,
): Uint8Array<ArrayBuffer> {
    const required = retainedBytes + appendedBytes;
    if (required <= buffer.byteLength) return buffer;

    const capacity = Math.min(
        hardLimit,
        buffer.byteLength + Math.max(buffer.byteLength, appendedBytes),
    );
    const grown = new Uint8Array(capacity);
    grown.set(buffer.subarray(0, retainedBytes));
    return grown;
}
