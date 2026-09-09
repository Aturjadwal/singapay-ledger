package domain

import (
	"context"
	"time"

	"github.com/21strive/redifu"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
)

// VerificationStatus represents the approval state of a verification
type VerificationStatus string

const (
	VerificationStatusPending  VerificationStatus = "PENDING"
	VerificationStatusApproved VerificationStatus = "APPROVED"
	VerificationStatusRejected VerificationStatus = "REJECTED"
)

// Verification represents a seller's KYC verification submission
type Verification struct {
	*redifu.Record `json:",inline" bson:",inline" db:"-"`
	AccountUUID    string // Which seller account this belongs to

	// Form data from KTP
	IdentityID string // KTP number
	Fullname   string
	BirthDate  time.Time
	Province   string
	City       string
	District   string
	PostalCode string

	// Photo storage paths (S3 keys)
	KTPPhotoURL    string // verification/ktp/{seller_id}/ktp.{ext}
	SelfiePhotoURL string // verification/kyc/{seller_id}/kyc-selfie.{ext}

	// Approval workflow
	Status          VerificationStatus
	ApprovedBy      string     // Admin/approver ID
	ApprovedAt      *time.Time // When approved or rejected
	RejectionReason string     // Why rejected (if applicable)

	// Metadata for additional info
	Metadata map[string]any
}

// VerificationRepository defines data access for verifications
type VerificationRepository interface {
	Save(ctx context.Context, verification *Verification) error
	GetByID(ctx context.Context, id string) (*Verification, error)
	GetByAccountID(ctx context.Context, accountID string) (*Verification, error)
	GetByIdentityID(ctx context.Context, identityID string) (*Verification, error)
	GetPendingVerifications(ctx context.Context, limit, offset int) ([]*Verification, error)
	UpdateStatus(ctx context.Context, id string, status VerificationStatus, approvedBy string, rejectionReason string) error
}

// NewVerification creates a new verification in PENDING status
func NewVerification(
	accountUUID string,
	identityID string,
	fullname string,
	birthDate time.Time,
	province string,
	city string,
	district string,
	postalCode string,
	ktpPhotoURL string,
	selfiePhotoURL string,
) (*Verification, error) {
	if accountUUID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "account_uuid is required", nil)
	}
	if identityID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "identity_id is required", nil)
	}
	if fullname == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "fullname is required", nil)
	}
	if ktpPhotoURL == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "ktp_photo_url is required", nil)
	}
	if selfiePhotoURL == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "selfie_photo_url is required", nil)
	}

	v := &Verification{
		AccountUUID:    accountUUID,
		IdentityID:     identityID,
		Fullname:       fullname,
		BirthDate:      birthDate,
		Province:       province,
		City:           city,
		District:       district,
		PostalCode:     postalCode,
		KTPPhotoURL:    ktpPhotoURL,
		SelfiePhotoURL: selfiePhotoURL,
		Status:         VerificationStatusPending,
		Metadata:       make(map[string]any),
	}
	redifu.InitRecord(v)

	// CRITICAL FIX: redifu.InitRecord initializes pointer fields to zero time instead of nil
	v.ApprovedAt = nil

	return v, nil
}

// IsPending checks if verification is awaiting review
func (v *Verification) IsPending() bool {
	return v.Status == VerificationStatusPending
}

// IsApproved checks if verification has been approved
func (v *Verification) IsApproved() bool {
	return v.Status == VerificationStatusApproved
}

// IsRejected checks if verification has been rejected
func (v *Verification) IsRejected() bool {
	return v.Status == VerificationStatusRejected
}

// Approve marks the verification as approved
func (v *Verification) Approve(approvedBy string) error {
	if !v.IsPending() {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "can only approve pending verifications", nil)
	}
	if approvedBy == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "approved_by is required", nil)
	}

	now := time.Now()
	v.Status = VerificationStatusApproved
	v.ApprovedBy = approvedBy
	v.ApprovedAt = &now
	v.RejectionReason = ""
	v.UpdatedAt = now
	return nil
}

// Reject marks the verification as rejected
func (v *Verification) Reject(rejectedBy string, reason string) error {
	if !v.IsPending() {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "can only reject pending verifications", nil)
	}
	if rejectedBy == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "rejected_by is required", nil)
	}
	if reason == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "rejection_reason is required", nil)
	}

	now := time.Now()
	v.Status = VerificationStatusRejected
	v.ApprovedBy = rejectedBy
	v.ApprovedAt = &now
	v.RejectionReason = reason
	v.UpdatedAt = now
	return nil
}
