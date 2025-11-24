package user

import (
	"context"

	"github.com/Bellorico323/gobid/internal/validator"
)

type CreateUserReq struct {
	UserName     string `json:"user_name"`
	PasswordHash []byte `json:"password_hash"`
	Email        string `json:"email"`
	Bio          string `json:"bio"`
}

func (req CreateUserReq) Valid(ctx context.Context) validator.Evaluator {
	var eval validator.Evaluator

	eval.CheckField(validator.NotBlank(req.UserName), "user_name", "this field cannot be empty")

	return eval
}
