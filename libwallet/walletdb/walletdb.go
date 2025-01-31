package walletdb

import (
	"errors"
	"log"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
	gormigrate "gopkg.in/gormigrate.v1"
)

type InvoiceState string

const (
	InvoiceStateRegistered InvoiceState = "registered"
	InvoiceStateUsed       InvoiceState = "used"
)

// TODO: probably rename to InvoiceSecrets or similar
type Invoice struct {
	gorm.Model
	Preimage      []byte
	PaymentHash   []byte
	PaymentSecret []byte
	KeyPath       string
	ShortChanId   uint64
	AmountSat     int64
	State         InvoiceState
	UsedAt        *time.Time
}

type DB struct {
	db *gorm.DB
}

func Open(path string) (*DB, error) {
	db, err := gorm.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}
	err = migrate(db)
	if err != nil {
		return nil, err
	}
	return &DB{db}, nil
}

func migrate(db *gorm.DB) error {
	opts := gormigrate.Options{
		UseTransaction: true,
	}
	m := gormigrate.New(db, &opts, []*gormigrate.Migration{
		{
			ID: "initial",
			Migrate: func(tx *gorm.DB) error {
				type Invoice struct {
					gorm.Model
					Preimage      []byte
					PaymentHash   []byte
					PaymentSecret []byte
					KeyPath       string
					ShortChanId   uint64
					State         string
					UsedAt        *time.Time
				}
				// This guard exists because at some point migrations were run outside a
				// transactional context and a user experimented problems with an invoices
				// table that was already created but whose migration had not been properly
				// recorded.
				if !tx.HasTable(&Invoice{}) {
					return tx.CreateTable(&Invoice{}).Error
				}
				return nil
			},
			Rollback: func(tx *gorm.DB) error {
				return tx.DropTable("invoices").Error
			},
		},
		{
			ID: "add amount to invoices table",
			Migrate: func(tx *gorm.DB) error {
				type Invoice struct {
					gorm.Model
					Preimage      []byte
					PaymentHash   []byte
					PaymentSecret []byte
					KeyPath       string
					ShortChanId   uint64
					AmountSat     int64
					State         string
					UsedAt        *time.Time
				}
				return tx.AutoMigrate(&Invoice{}).Error
			},
			Rollback: func(tx *gorm.DB) error {
				return tx.Table("invoices").DropColumn(gorm.ToColumnName("AmountSat")).Error
			},
		},
	})
	return m.Migrate()
}

func (d *DB) CreateInvoice(invoice *Invoice) error {
	// uint64 values with high bit set are not supported, we will
	// have to convert back and forth
	invoice.ShortChanId = invoice.ShortChanId & 0x7FFFFFFFFFFFFFFF
	res := d.db.Create(invoice)
	invoice.ShortChanId = invoice.ShortChanId | (1 << 63)
	return res.Error
}

func (d *DB) SaveInvoice(invoice *Invoice) error {
	// uint64 values with high bit set are not supported, we will
	// have to convert back and forth
	invoice.ShortChanId = invoice.ShortChanId & 0x7FFFFFFFFFFFFFFF
	res := d.db.Save(invoice)
	invoice.ShortChanId = invoice.ShortChanId | (1 << 63)
	return res.Error
}

func (d *DB) FindFirstUnusedInvoice() (*Invoice, error) {
	var invoice Invoice
	if res := d.db.Where(&Invoice{State: InvoiceStateRegistered}).First(&invoice); res.Error != nil {

		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}

		return nil, res.Error
	}
	invoice.ShortChanId = invoice.ShortChanId | (1 << 63)
	return &invoice, nil
}

func (d *DB) CountUnusedInvoices() (int, error) {
	var count int
	if res := d.db.Model(&Invoice{}).Where(&Invoice{State: InvoiceStateRegistered}).Count(&count); res.Error != nil {
		return 0, res.Error
	}
	return count, nil
}

func (d *DB) FindByPaymentHash(hash []byte) (*Invoice, error) {
	var invoice Invoice
	if res := d.db.Where(&Invoice{PaymentHash: hash}).First(&invoice); res.Error != nil {
		return nil, res.Error
	}
	invoice.ShortChanId = invoice.ShortChanId | (1 << 63)
	return &invoice, nil
}

func (d *DB) Close() {
	err := d.db.Close()
	if err != nil {
		log.Printf("error closing the db: %v", err)
	}
}
