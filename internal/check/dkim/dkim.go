package dkim

import (
	"bytes"
	"context"
	"errors"
	"io"
	nettextproto "net/textproto"
	"runtime/trace"
	"strings"

	"github.com/emersion/go-message/textproto"
	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/foxcpp/maddy/internal/buffer"
	"github.com/foxcpp/maddy/internal/check"
	"github.com/foxcpp/maddy/internal/config"
	"github.com/foxcpp/maddy/internal/exterrors"
	"github.com/foxcpp/maddy/internal/log"
	"github.com/foxcpp/maddy/internal/module"
	"github.com/foxcpp/maddy/internal/target"
)

type Check struct {
	instName string
	log      log.Logger

	requiredFields  map[string]struct{}
	allowBodySubset bool
	brokenSigAction check.FailAction
	noSigAction     check.FailAction
	failOpen        bool
}

func New(_, instName string, _, inlineArgs []string) (module.Module, error) {
	if len(inlineArgs) != 0 {
		return nil, errors.New("verify_dkim: inline arguments are not used")
	}
	return &Check{
		instName: instName,
		log:      log.Logger{Name: "verify_dkim"},
	}, nil
}

func (c *Check) Init(cfg *config.Map) error {
	var requiredFields []string

	cfg.Bool("debug", true, false, &c.log.Debug)
	cfg.StringList("required_fields", false, false, []string{"From", "Subject"}, &requiredFields)
	cfg.Bool("allow_body_subset", false, false, &c.allowBodySubset)
	cfg.Bool("fail_open", false, false, &c.failOpen)
	cfg.Custom("broken_sig_action", false, false,
		func() (interface{}, error) {
			return check.FailAction{}, nil
		}, check.FailActionDirective, &c.brokenSigAction)
	cfg.Custom("no_sig_action", false, false,
		func() (interface{}, error) {
			return check.FailAction{}, nil
		}, check.FailActionDirective, &c.noSigAction)
	_, err := cfg.Process()
	if err != nil {
		return err
	}

	c.requiredFields = make(map[string]struct{})
	for _, field := range requiredFields {
		c.requiredFields[nettextproto.CanonicalMIMEHeaderKey(field)] = struct{}{}
	}

	return nil
}

func (c *Check) Name() string {
	return "verify_dkim"
}

func (c *Check) InstanceName() string {
	return c.instName
}

type dkimCheckState struct {
	c       *Check
	msgMeta *module.MsgMetadata
	log     log.Logger
}

func (d *dkimCheckState) CheckConnection(ctx context.Context) module.CheckResult {
	return module.CheckResult{}
}

func (d *dkimCheckState) CheckSender(ctx context.Context, mailFrom string) module.CheckResult {
	return module.CheckResult{}
}

func (d *dkimCheckState) CheckRcpt(ctx context.Context, rcptTo string) module.CheckResult {
	return module.CheckResult{}
}

func (d *dkimCheckState) CheckBody(ctx context.Context, header textproto.Header, body buffer.Buffer) module.CheckResult {
	defer trace.StartRegion(ctx, "verify_dkim/CheckBody").End()

	if !header.Has("DKIM-Signature") {
		if d.c.noSigAction.Reject || d.c.noSigAction.Quarantine {
			d.log.Printf("no signatures present")
		} else {
			d.log.Debugf("no signatures present")
		}
		return d.c.noSigAction.Apply(module.CheckResult{
			Reason: &exterrors.SMTPError{
				Code:         550,
				EnhancedCode: exterrors.EnhancedCode{5, 7, 20},
				Message:      "No DKIM signatures",
				CheckName:    "verify_dkim",
			},
			AuthResult: []authres.Result{
				&authres.DKIMResult{
					Value: authres.ResultNone,
				},
			},
		})
	}

	b := bytes.Buffer{}
	_ = textproto.WriteHeader(&b, header)
	bodyRdr, err := body.Open()
	if err != nil {
		return module.CheckResult{
			Reject: true,
			Reason: exterrors.WithTemporary(
				exterrors.WithFields(err, map[string]interface{}{
					"check":    "verify_dkim",
					"smtp_msg": "Internal I/O error",
				}),
				true,
			),
		}
	}

	verifications, err := dkim.Verify(io.MultiReader(&b, bodyRdr))
	if err != nil {
		return module.CheckResult{
			Reject: true,
			Reason: exterrors.WithTemporary(
				exterrors.WithFields(err, map[string]interface{}{
					"check":    "verify_dkim",
					"smtp_msg": "Internal error during policy check",
				}),
				true,
			),
		}
	}

	goodSigs := false

	res := module.CheckResult{AuthResult: make([]authres.Result, 0, len(verifications))}
	for _, verif := range verifications {
		val := authres.ResultValue(authres.ResultPass)
		reason := ""
		if verif.Err != nil {
			val = authres.ResultFail

			reason = strings.TrimPrefix(verif.Err.Error(), "dkim: ")
			if !d.c.brokenSigAction.Reject || !d.c.brokenSigAction.Quarantine {
				d.log.DebugMsg("bad signature", "domain", verif.Domain, "identifier", verif.Identifier)
			}
			if dkim.IsPermFail(err) {
				val = authres.ResultPermError
			}
			if dkim.IsTempFail(err) {
				if !d.c.failOpen {
					return module.CheckResult{
						Reject: true,
						Reason: &exterrors.SMTPError{
							Code:         421,
							EnhancedCode: exterrors.EnhancedCode{4, 7, 20},
							Message:      "Temporary error during DKIM verification",
							CheckName:    "verify_dkim",
							Err:          err,
						},
					}
				}
				val = authres.ResultTempError
			}

			res.AuthResult = append(res.AuthResult, &authres.DKIMResult{
				Value:      val,
				Reason:     reason,
				Domain:     verif.Domain,
				Identifier: verif.Identifier,
			})
			continue
		}

		goodSigs = true
		d.log.DebugMsg("good signature", "domain", verif.Domain, "identifier", verif.Identifier)

		signedFields := make(map[string]struct{}, len(verif.HeaderKeys))
		for _, field := range verif.HeaderKeys {
			signedFields[nettextproto.CanonicalMIMEHeaderKey(field)] = struct{}{}
		}
		for field := range d.c.requiredFields {
			if _, ok := signedFields[field]; !ok {
				val = authres.ResultPermError
				reason = "some header fields are not signed"
			}
		}

		if verif.BodyLength >= 0 && !d.c.allowBodySubset {
			val = authres.ResultPermError
			reason = "body limit it used"
		}

		res.AuthResult = append(res.AuthResult, &authres.DKIMResult{
			Value:      val,
			Reason:     reason,
			Domain:     verif.Domain,
			Identifier: verif.Identifier,
		})
	}

	if !goodSigs {
		res.Reason = &exterrors.SMTPError{
			Code:         550,
			EnhancedCode: exterrors.EnhancedCode{5, 7, 20},
			Message:      "No passing DKIM signatures",
			CheckName:    "verify_dkim",
		}
		return d.c.brokenSigAction.Apply(res)
	}
	return res
}

func (d *dkimCheckState) Name() string {
	return "verify_dkim"
}

func (d *dkimCheckState) Close() error {
	return nil
}

func (c *Check) CheckStateForMsg(ctx context.Context, msgMeta *module.MsgMetadata) (module.CheckState, error) {
	return &dkimCheckState{
		c:       c,
		msgMeta: msgMeta,
		log:     target.DeliveryLogger(c.log, msgMeta),
	}, nil
}

func init() {
	module.Register("verify_dkim", New)
}
