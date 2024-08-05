package service

import (
	"context"
	"github.com/dwprz/prasorganic-auth-service/src/common/errors"
	"github.com/dwprz/prasorganic-auth-service/src/core/grpc/grpc"
	"github.com/dwprz/prasorganic-auth-service/src/infrastructure/config"
	"github.com/dwprz/prasorganic-auth-service/src/interface/cache"
	"github.com/dwprz/prasorganic-auth-service/src/interface/helper"
	"github.com/dwprz/prasorganic-auth-service/src/interface/service"
	"github.com/dwprz/prasorganic-auth-service/src/model/dto"
	"github.com/dwprz/prasorganic-auth-service/src/model/entity"
	pb "github.com/dwprz/prasorganic-proto/protogen/user"
	"github.com/go-playground/validator/v10"
	"github.com/jinzhu/copier"
	gonanoid "github.com/matoous/go-nanoid/v2"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
)

type AuthImpl struct {
	grpcClient *grpc.Client
	otpService service.Otp
	validate   *validator.Validate
	authCache  cache.Auth
	logger     *logrus.Logger
	conf       *config.Config
	helper     helper.Helper
}

func NewAuth(gc *grpc.Client, os service.Otp, v *validator.Validate, ac cache.Auth,
	l *logrus.Logger, conf *config.Config, h helper.Helper) service.Auth {
	return &AuthImpl{
		grpcClient: gc,
		otpService: os,
		validate:   v,
		authCache:  ac,
		logger:     l,
		conf:       conf,
		helper:     h,
	}
}

func (a *AuthImpl) Register(ctx context.Context, data *dto.RegisterReq) (string, error) {
	if err := a.validate.Struct(data); err != nil {
		return "", err
	}

	result, err := a.grpcClient.User.FindByEmail(ctx, data.Email)

	if err != nil {
		return "", err
	}

	if result.Data != nil {
		return "", &errors.Response{HttpCode: 409, Message: "user already exists"}
	}

	encryptPwd, err := bcrypt.GenerateFromPassword([]byte(data.Password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}

	data.Password = string(encryptPwd)

	go a.otpService.Send(ctx, data.Email)
	go a.authCache.CacheRegisterReq(ctx, data)

	return data.Email, nil
}

func (a *AuthImpl) VerifyRegister(ctx context.Context, data *dto.VerifyOtpReq) error {
	if err := a.otpService.Verify(ctx, data); err != nil {
		return err
	}

	registerReq := a.authCache.FindRegisterReq(ctx, data.Email)
	if registerReq == nil {
		return &errors.Response{HttpCode: 404, Message: "register request not found"}
	}

	req := new(pb.RegisterRequest)
	if err := copier.Copy(req, registerReq); err != nil {
		return err
	}

	userId, err := gonanoid.New()
	if err != nil {
		return err
	}

	req.UserId = userId

	if err = a.grpcClient.User.Create(ctx, req); err != nil {
		return err
	}

	go a.authCache.DeleteRegisterReq(context.Background(), data.Email)

	return nil
}

func (a *AuthImpl) LoginWithGoogle(ctx context.Context, data *dto.LoginWithGoogleReq) (*dto.LoginWithGoogleRes, error) {
	if err := a.validate.Struct(data); err != nil {
		return nil, err
	}

	req := new(pb.LoginWithGoogleRequest)
	if err := copier.Copy(req, data); err != nil {
		return nil, err
	}

	res, err := a.grpcClient.User.Upsert(ctx, req)
	if err != nil {
		return nil, err
	}

	user := new(dto.LoginWithGoogleRes)
	if err := copier.Copy(user, res); err != nil {
		return nil, err
	}

	return user, nil
}

func (a *AuthImpl) Login(ctx context.Context, data *dto.LoginReq) (*dto.LoginRes, error) {
	if err := a.validate.Struct(data); err != nil {
		return nil, err
	}

	res, err := a.grpcClient.User.FindByEmail(ctx, data.Email)
	if err != nil {
		return nil, err
	}

	if res.Data == nil {
		return nil, &errors.Response{HttpCode: 404, Message: "email is invalid"}
	}

	if err := bcrypt.CompareHashAndPassword([]byte(res.Data.Password), []byte(data.Password)); err != nil {
		return nil, &errors.Response{HttpCode: 401, Message: "password is invalid"}
	}

	accessToken, err := a.helper.GenerateAccessToken(res.Data.UserId, res.Data.Email, res.Data.Role)
	if err != nil {
		return nil, err
	}

	refreshToken, err := a.helper.GenerateRefreshToken()
	if err != nil {
		return nil, err
	}

	go a.grpcClient.User.AddRefreshToken(ctx, &pb.AddRefreshToken{
		Email: data.Email,
		Token: refreshToken,
	})

	user := new(entity.SanitizedUser)
	if err := copier.Copy(user, res.Data); err != nil {
		return nil, err
	}

	user.CreatedAt = res.Data.CreatedAt.AsTime()
	user.UpdatedAt = res.Data.UpdatedAt.AsTime()

	return &dto.LoginRes{
		Data: user,
		Tokens: &entity.Tokens{
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
		},
	}, nil
}

func (a *AuthImpl) RefreshToken(ctx context.Context, refreshToken string) (*entity.Tokens, error) {
	res, err := a.grpcClient.User.FindByRefreshToken(ctx, &pb.RefreshToken{
		Token: refreshToken,
	})

	if err != nil {
		return nil, err
	}

	accessToken, err := a.helper.GenerateAccessToken(res.Data.UserId, res.Data.Email, res.Data.Role)
	if err != nil {
		return nil, err
	}

	return &entity.Tokens{
		AccessToken: accessToken,
	}, nil
}

func (a *AuthImpl) SetNullRefreshToken(ctx context.Context, refreshToken string) error {
	go a.grpcClient.User.SetNullRefreshToken(ctx, refreshToken)

	return nil
}
