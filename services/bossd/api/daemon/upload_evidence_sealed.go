package server

import (
	"io"

	"connectrpc.com/connect"
	"github.com/recurser/bossd/internal/proofseal"
)

const hostedProofSealing = true

type hostedObject struct {
	name string
	size int64
	body func() ([]byte, error)
}

func hostedObjects(files []evidenceUploadFile, key []byte) []hostedObject {
	objects := make([]hostedObject, 0, len(files)+2)
	for _, f := range files {
		if key == nil || f.name == evidencePlayerName {
			objects = append(objects, hostedObject{name: f.name, size: f.size, body: func() ([]byte, error) { return readEvidenceFile(f) }})
			continue
		}
		name := proofseal.SealedName(f.name)
		objects = append(objects, hostedObject{name: name, size: proofseal.SealedSize(f.size), body: func() ([]byte, error) { return sealEvidenceFile(key, name, f) }})
	}
	if key != nil {
		objects = append(objects, embeddedObject(proofseal.ShimName, proofseal.Shim()), embeddedObject(proofseal.LoaderName, proofseal.Loader()))
	}
	return objects
}

func embeddedObject(name string, body []byte) hostedObject {
	return hostedObject{name: name, size: int64(len(body)), body: func() ([]byte, error) { return body, nil }}
}

func readEvidenceFile(f evidenceUploadFile) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(f.file, f.size+1))
	if err != nil || int64(len(body)) != f.size {
		return nil, evidenceFailure(connect.CodeUnavailable, "object-changed", "evidence object changed while uploading", err)
	}
	return body, nil
}

func sealEvidenceFile(key []byte, name string, f evidenceUploadFile) ([]byte, error) {
	plain, err := readEvidenceFile(f)
	if err != nil {
		return nil, err
	}
	sealed, err := proofseal.Seal(key, name, plain)
	if err != nil {
		return nil, evidenceFailure(connect.CodeInternal, "seal-failed", "evidence object could not be sealed", err)
	}
	return sealed, nil
}
