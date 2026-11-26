# The contact form on the site

```yaml
modules:
  forms:
    settings:
      notify_email: [sales@acme.example]     # who is emailed about every new message
email:
  relay: { host: smtp.example.com, from: no-reply@acme.example }
site:
  contact:                                   # every field is optional; `contact: {}` works
    title: Contact us
    text: Send us a message.
    form: contact                            # a form the forms module serves
    button: Send us a message
```

`can init` writes the `forms` module and a `contact` section (not for `provider.type: static`).

The section is a button that opens `/_forms/<form>/form`, the page the forms module serves on the
site's own origin. The page carries the timing token, so there is no script on the site and its
Content-Security-Policy stays `default-src 'none'` with no `script-src` and no frames. The honeypot,
the timing check, the rate limit and the same-origin check apply as to any submission.

## Email about new messages

Two settings, both in company.yaml:

- `modules.forms.settings.notify_email`: the recipients. Without it, messages are only kept: nobody is
  told ([reading them](#reading-the-messages)).
- `email.relay`: the SMTP relay they are sent through. The credentials are not in company.yaml:
  `make up SMTP_CREDENTIALS=smtp.env` ([email.md](email.md)).

## Reading the messages

Every message is kept by the forms module, whether or not anyone is emailed (`can validate` and
`can up` warn when nobody is). Read them with its leads API, with `kubectl` pointed at the server
([make-up.md](make-up.md)):

```sh
kubectl -n mod-forms port-forward svc/forms-api 8081:8081 &
TOKEN=$(kubectl -n mod-forms get secret can-module -o go-template='{{index .data "CAN_FORMS_API_READ_TOKEN" | base64decode}}')
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8081/v1/leads
```

The admin UI's `/leads` page shows them only where can-core has been given a leads credential
(`CAN_LEADS_TOKEN_FILE`, [admin-ui.md](admin-ui.md)); `make up` does not set one yet.

## Checks

`can validate`, `can site build` and `can up` refuse a `form` the forms module does not have (read
from `forms/chart/values.yaml` in the modules directory), or one with a CAPTCHA, which has no page.
Without a modules directory (a release archive, `go install`) `can` uses the module manifests built
into it, and the shipped `contact` form.

| company.yaml | The section | Said |
|---|---|---|
| `modules.forms` enabled | on the site | nothing, or a warning when nobody is emailed |
| `forms` not listed | on the site (`can up` adds the module); left out with `can up --no-default-bundle` | warning from `validate` and `site build` |
| `enabled: false`, or `expose.public: false` | left out, so there is no dead link | warning |

The link works where `/_forms` is routed to the forms module: after `make up`, and in the Compose
stack. `can site serve` previews the site only, so the button leads to "not found" there. A site
published elsewhere needs the same route.

With a `contact` section the older `site.cta` block, if kept, moves from the `#contact` anchor to
`#cta`.
