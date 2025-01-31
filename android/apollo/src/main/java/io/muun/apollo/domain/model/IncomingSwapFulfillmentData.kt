package io.muun.apollo.domain.model

class IncomingSwapFulfillmentData(
        val fulfillmentTx: ByteArray,
        val muunSignature:ByteArray,
        val outputPath: String,
        val outputVersion: Int
) {
    fun toLibwalletModel(): libwallet.IncomingSwapFulfillmentData {
        val data = libwallet.IncomingSwapFulfillmentData()

        data.fulfillmentTx = fulfillmentTx
        data.muunSignature = muunSignature
        data.outputPath = outputPath
        data.outputVersion = outputVersion.toLong()

        // unused for now but should eventually be provided by houston
        data.htlcBlock = byteArrayOf()
        data.confirmationTarget = 0
        data.blockHeight = 0
        data.merkleTree = byteArrayOf()

        return data
    }
}