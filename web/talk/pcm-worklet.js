// Runs in AudioWorkletGlobalScope.
//
// Two modes, selected by the AudioContext's sample rate:
//   - 16 kHz context: legacy path — accumulate 320 samples (20 ms), convert
//     to Int16 PCM, send as ArrayBuffer. Used when RNNoise is off/unavailable
//     and the caller stays at 16 kHz.
//   - 48 kHz context: RNNoise path — accumulate 480 samples (10 ms) of Float32,
//     send as Float32Array to main. Main runs RNNoise.processFrame, then
//     downsamples 3:1 to 16 kHz Int16 for the wire.
//
// The processor picks its frame size and payload format at construct time from
// `options.processorOptions.frameSize` (samples) and `options.processorOptions.format`
// ('int16' | 'float32'). Default: {frameSize: 320, format: 'int16'} to preserve the
// old behavior for any caller that instantiates without options.
class PCMCapturer extends AudioWorkletProcessor {
  constructor(options) {
    super()
    const opts = (options && options.processorOptions) || {}
    this.frameSize = opts.frameSize | 0 || 320
    this.format = opts.format === 'float32' ? 'float32' : 'int16'
    this.frame = new Float32Array(this.frameSize)
    this.idx = 0
  }
  process(inputs) {
    const input = inputs[0] && inputs[0][0]
    if (!input) return true
    for (let i = 0; i < input.length; i++) {
      this.frame[this.idx++] = input[i]
      if (this.idx === this.frame.length) {
        if (this.format === 'float32') {
          // Send a copy; the same underlying frame buffer is reused on the next fill.
          const copy = new Float32Array(this.frame)
          this.port.postMessage(copy.buffer, [copy.buffer])
        } else {
          const int16 = new Int16Array(this.frame.length)
          for (let j = 0; j < this.frame.length; j++) {
            let s = this.frame[j]
            if (s > 1) s = 1
            else if (s < -1) s = -1
            int16[j] = s < 0 ? s * 32768 : s * 32767
          }
          this.port.postMessage(int16.buffer, [int16.buffer])
        }
        this.idx = 0
      }
    }
    return true
  }
}
registerProcessor('pcm-capturer', PCMCapturer)
