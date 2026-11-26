{{- /*
Website leads become conversations (specs/069-lead-adapters). FreeScout has no API in its free
core, so this listener runs beside it, inside the pod, with FreeScout's own models. can-core is its
only intended caller: no route reaches the port, and every request must carry can-core's signature
made with this module's webhook secret. Nothing a request holds is written to a log: an error is
named by its class only, because a database error's message quotes the values.
*/ -}}
{{- define "support.leads" -}}
<?php
ini_set('display_errors', '0');

function can_answer($status, $body = []) {
    http_response_code($status);
    header('Content-Type: application/json');
    echo json_encode((object) $body);
    exit;
}

$path = parse_url($_SERVER['REQUEST_URI'], PHP_URL_PATH);
if ($_SERVER['REQUEST_METHOD'] !== 'POST' || !in_array($path, ['/hooks/lead', '/hooks/lead-erased'], true)) {
    can_answer(404);
}
$secret = (string) getenv('CAN_WEBHOOK_SECRET');
$raw = (string) file_get_contents('php://input', false, null, 0, 1 << 19);
$ts = (string) ($_SERVER['HTTP_X_CAN_TIMESTAMP'] ?? '');
$sig = (string) ($_SERVER['HTTP_X_CAN_SIGNATURE'] ?? '');
if ($secret === '' || !ctype_digit($ts) || abs(time() - (int) $ts) > 300
    || !hash_equals('v1=' . hash_hmac('sha256', $ts . '.' . $raw, $secret), $sig)) {
    can_answer(401);
}
$in = json_decode($raw, true);
$lead = is_array($in) ? (string) ($in['lead_id'] ?? '') : '';
if (!preg_match('/^ld_[0-9a-f]{24}$/', $lead)) {
    can_answer(400);
}

try {
    require '/var/www/html/vendor/autoload.php';
    $app = require '/var/www/html/bootstrap/app.php';
    $app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap();

    $id = (int) \App\Option::get("can_lead_$lead", 0, true, false);
    $conversation = $id ? \App\Conversation::find($id) : null;

    if ($path === '/hooks/lead-erased') {
        if (!$conversation) {
            can_answer(200, ['conversation' => 0]);
        }
        if (!\App\Option::get("can_lead_erased_$lead", 0, true, false)) {
            $admin = \App\User::where('role', \App\User::ROLE_ADMIN)->orderBy('id')->first();
            if (!$admin) {
                can_answer(503);
            }
            \DB::transaction(function () use ($conversation, $admin, $lead) {
                \App\Thread::createExtended([
                    'type' => \App\Thread::TYPE_NOTE,
                    'created_by_user_id' => $admin->id,
                    'body' => '<p>The website lead this conversation was made from has been erased on request (' . htmlspecialchars($lead) . ', ' . gmdate('Y-m-d') . ').</p>'
                        . '<p><strong>This conversation still holds a copy</strong>, and so does the customer record. If the request covers them, delete the conversation (and the customer) here, then empty the Deleted folder.</p>',
                ], $conversation, $conversation->customer);
                // Back among the open ones: somebody has to decide.
                $conversation->status = \App\Conversation::STATUS_ACTIVE;
                $conversation->updateFolder();
                $conversation->save();
                $conversation->mailbox->updateFoldersCounters();
                \App\Option::set("can_lead_erased_$lead", 1);
            });
        }
        can_answer(200, ['conversation' => $conversation->id]);
    }

    if ($conversation) {
        can_answer(200, ['conversation' => $conversation->id]);
    }
    $form = (string) ($in['form_id'] ?? '');
    if (!preg_match('/^[a-z][a-z0-9-]{0,40}$/', $form)) {
        can_answer(400);
    }
    $mailbox = \App\Mailbox::find((int) \App\Option::get('can_mailbox_id', 0, true, false));
    if (!$mailbox) {
        can_answer(503);
    }
    $email = trim((string) ($in['email'] ?? ''));
    $name = trim((string) ($in['name'] ?? ''));
    $fields = is_array($in['fields'] ?? null) ? $in['fields'] : [];

    $id = \DB::transaction(function () use ($lead, $form, $mailbox, $email, $name, $fields) {
        $parts = preg_split('/\s+/', $name, 2);
        $data = ['first_name' => mb_substr($parts[0] ?? '', 0, 60), 'last_name' => mb_substr($parts[1] ?? '', 0, 60)];
        $customer = $email !== '' ? \App\Customer::create($email, $data) : null;
        $customer = $customer ?: \App\Customer::createWithoutEmail($data);

        $body = '<p><em>Sent with the website form &quot;' . $form . '&quot;. The sender typed the address; it has not been verified.</em></p>';
        foreach ($fields as $key => $value) {
            $body .= '<p><strong>' . htmlspecialchars((string) $key) . '</strong><br>' . nl2br(htmlspecialchars((string) $value)) . '</p>';
        }

        $conversation = new \App\Conversation();
        $conversation->type = \App\Conversation::TYPE_EMAIL;
        $conversation->subject = "Website enquiry ($form)";
        $conversation->mailbox_id = $mailbox->id;
        $conversation->source_via = \App\Conversation::PERSON_CUSTOMER;
        $conversation->source_type = \App\Conversation::SOURCE_TYPE_API;
        $conversation->customer_id = $customer->id;
        $conversation->customer_email = (string) $customer->getMainEmail();
        $conversation->state = \App\Conversation::STATE_PUBLISHED;
        $conversation->status = \App\Conversation::STATUS_ACTIVE;
        $conversation->imported = 0;
        $conversation->preview = '';
        // Never an automatic reply: anybody can type anybody's address into a form.
        $conversation->setMeta('ar_off', true);
        $conversation->updateFolder();
        $conversation->save();

        if (!\App\Thread::createExtended(['type' => \App\Thread::TYPE_CUSTOMER, 'body' => $body], $conversation, $customer)) {
            throw new \RuntimeException('no thread');
        }
        $conversation->mailbox->updateFoldersCounters();
        \App\Option::set("can_lead_$lead", $conversation->id);
        return $conversation->id;
    });
    can_answer(200, ['conversation' => $id]);
} catch (\Throwable $e) {
    error_log('can: lead ' . $lead . ': ' . get_class($e));
    can_answer(500);
}
{{- end -}}
