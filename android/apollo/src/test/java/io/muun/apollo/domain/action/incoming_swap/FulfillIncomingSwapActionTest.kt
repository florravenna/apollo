package io.muun.apollo.domain.action.incoming_swap

import com.nhaarman.mockitokotlin2.*
import io.muun.apollo.BaseTest
import io.muun.apollo.data.db.incoming_swap.IncomingSwapDao
import io.muun.apollo.data.db.operation.OperationDao
import io.muun.apollo.data.external.Gen
import io.muun.apollo.data.external.Globals
import io.muun.apollo.data.net.HoustonClient
import io.muun.apollo.data.preferences.KeysRepository
import io.muun.apollo.domain.libwallet.errors.UnfulfillableIncomingSwapError
import io.muun.apollo.domain.model.IncomingSwap
import io.muun.apollo.domain.model.IncomingSwapFulfillmentData
import io.muun.apollo.domain.model.Operation
import io.muun.common.api.error.ErrorCode
import io.muun.common.crypto.hd.PrivateKey
import io.muun.common.exception.HttpException
import io.muun.common.utils.Hashes
import io.muun.common.utils.RandomGenerator
import org.bitcoinj.params.RegTestParams
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.mockito.Mock
import org.mockito.Mockito
import org.mockito.Mockito.verify
import org.mockito.junit.MockitoJUnitRunner
import rx.Completable
import rx.Observable
import rx.Single

@RunWith(MockitoJUnitRunner::class)
class FulfillIncomingSwapActionTest: BaseTest() {

    @Mock
    private lateinit var houstonClient: HoustonClient

    @Mock
    private lateinit var operationDao: OperationDao

    @Mock
    private lateinit var keysRepository: KeysRepository

    @Mock
    private lateinit var incomingSwapDao: IncomingSwapDao

    private lateinit var action: FulfillIncomingSwapAction

    private val params = RegTestParams.get()

    @Before
    fun setUp() {
        whenever(Globals.INSTANCE.network).thenReturn(params)

        whenever(keysRepository.basePrivateKey)
            .thenReturn(Observable.just(PrivateKey.getNewRootPrivateKey(params)))

        whenever(keysRepository.baseMuunPublicKey)
            .thenReturn(PrivateKey.getNewRootPrivateKey(params).publicKey)

        whenever(incomingSwapDao.store(any()))
            .then { invocation -> Observable.just(
                invocation.getArgument<io.muun.apollo.domain.model.IncomingSwap>(0)
            ) }

        whenever(operationDao.store(any()))
            .then { invocation -> Observable.just(invocation.getArgument<Operation>(0)) }

        action = FulfillIncomingSwapAction(
            houstonClient,
            operationDao,
            keysRepository,
            params,
            incomingSwapDao
        )
    }

    @Test(expected = java.lang.RuntimeException::class)
    fun unknownSwap() {
        whenever(operationDao.fetchByIncomingSwapUuid(eq("fake")))
            .thenReturn(Observable.error(RuntimeException()))

        action.actionNow("fake")
    }

    @Test
    fun unfulfillableSwap() {
        val swap = Mockito.spy(Gen.incomingSwap())
        val operation = Gen.operation(swap)

        whenever(operationDao.fetchByIncomingSwapUuid(eq(swap.houstonUuid)))
            .thenReturn(Observable.just(operation))

        doThrow(UnfulfillableIncomingSwapError(swap.houstonUuid, java.lang.RuntimeException()))
                .`when`(swap).verifyFulfillable(any(), eq(params))

        whenever(houstonClient.fetchFulfillmentData(eq(swap.houstonUuid)))
            .thenReturn(Single.just(null))

        whenever(houstonClient.expireInvoice(eq(swap.paymentHash)))
            .thenReturn(Completable.complete())

        action.action(swap.houstonUuid).toBlocking().subscribe()

        verify(houstonClient).expireInvoice(eq(swap.paymentHash))
        verify(houstonClient, never()).pushFulfillmentTransaction(
            any(), any()
        )
    }

    @Test
    fun onchainFulfillment() {
        val preimage = RandomGenerator.getBytes(32)
        val swap = Mockito.spy(Gen.incomingSwap())
        val operation = Gen.operation(swap)
        val fulfillmentData = IncomingSwapFulfillmentData(
            ByteArray(0),
            ByteArray(0),
            "",
            1
        )

        whenever(operationDao.fetchByIncomingSwapUuid(eq(swap.houstonUuid)))
            .thenReturn(Observable.just(operation))

        doNothing().whenever(swap).verifyFulfillable(
            any(),
            eq(params)
        )

        whenever(houstonClient.fetchFulfillmentData(eq(swap.houstonUuid)))
            .thenReturn(Single.just(fulfillmentData))

        doReturn(IncomingSwap.FulfillmentResult(ByteArray(0), preimage))
                .`when`(swap).fulfill(eq(fulfillmentData), any(), any(), eq(params))

        whenever(houstonClient.pushFulfillmentTransaction(eq(swap.houstonUuid), any()))
            .thenReturn(Completable.complete())

        action.action(swap.houstonUuid).toBlocking().subscribe()

        verify(houstonClient, never()).expireInvoice(eq(swap.paymentHash))
        verify(houstonClient).pushFulfillmentTransaction(
            eq(swap.houstonUuid), any()
        )
    }

    @Test
    fun fullDebtFulfilment() {
        val preimage = RandomGenerator.getBytes(32)
        val swap = Mockito.spy(Gen.incomingSwap(paymentHash = Hashes.sha256(preimage), htlc = null))
        val operation = Gen.operation(swap)

        whenever(operationDao.fetchByIncomingSwapUuid(eq(swap.houstonUuid)))
            .thenReturn(Observable.just(operation))

        doNothing().whenever(swap).verifyFulfillable(
            any(),
            eq(params)
        )

        doReturn(IncomingSwap.FulfillmentResult(byteArrayOf(), preimage))
                .`when`(swap).fulfillFullDebt()

        whenever(houstonClient.fulfillIncomingSwap(eq(swap.houstonUuid), eq(preimage)))
            .thenReturn(Completable.complete())

        action.action(swap.houstonUuid).toBlocking().subscribe()

        verify(houstonClient).fulfillIncomingSwap(eq(swap.houstonUuid), eq(preimage))
        verify(houstonClient, never()).expireInvoice(eq(swap.paymentHash))
        verify(houstonClient, never()).fetchFulfillmentData(eq(swap.houstonUuid))
        verify(houstonClient, never()).pushFulfillmentTransaction(
            eq(swap.houstonUuid), any()
        )
    }

    @Test
    fun onchainAlreadyFulfilled() {
        val swap = Mockito.spy(Gen.incomingSwap())
        val operation = Gen.operation(swap)

        whenever(operationDao.fetchByIncomingSwapUuid(eq(swap.houstonUuid)))
            .thenReturn(Observable.just(operation))

        doNothing().whenever(swap).verifyFulfillable(
            any(),
            eq(params)
        )

        whenever(houstonClient.fetchFulfillmentData(eq(swap.houstonUuid)))
            .thenReturn(Single.error(HttpException(ErrorCode.INCOMING_SWAP_ALREADY_FULFILLED)))

        action.action(swap.houstonUuid).toBlocking().subscribe()

        verify(houstonClient, never()).expireInvoice(eq(swap.paymentHash))
        verify(houstonClient, never()).pushFulfillmentTransaction(
            eq(swap.houstonUuid), any()
        )
    }
}