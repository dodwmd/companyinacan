{{- /*
What company.yaml says about mail, written into FreeScout, and a health check of it
(specs/059-support-mailbox). FreeScout keeps mail settings in its database and has no command for
them, so this script boots the application and uses its own models. It reads everything from the
container's environment; no password is in it. It is part of the pod's command (deployment.yaml),
so that a change of it restarts the pod.
*/ -}}
{{- define "support.provision" -}}
<?php
// Usage: php can-support.php apply | status
require '/var/www/html/vendor/autoload.php';
$app = require '/var/www/html/bootstrap/app.php';
$app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap();

use App\Mailbox;
use App\Option;
use App\User;

class ConfigRefused extends Exception {}

function env_str($k) { return (string) getenv($k); }

// An option as the database has it now, not as this process cached it.
function opt($name) { return Option::get($name, '', true, false); }

// What went wrong, in words of our own. The libraries' messages are never shown: an SMTP error
// quotes the last command, which can be the password in base64.
function reason($msg) {
    foreach ([
        '/auth/i' => 'the server refused the user name or password',
        '/getaddrinfo|resolve/i' => 'the host name does not resolve',
        '/certificate|SSL operation|enable TLS|STARTTLS|crypto/i' => 'TLS failed (certificate not trusted, or no TLS on this port)',
        '/timed? ?out/i' => 'the server did not answer in time',
        '/connect|refused|unreachable|no response/i' => 'cannot connect to the host and port',
        '/folder/i' => 'the folder does not exist',
    ] as $pattern => $words) {
        if (preg_match($pattern, (string) $msg)) {
            return $words;
        }
    }
    return 'it failed (see Manage, Logs in FreeScout)';
}

// The company relay as FreeScout's system mail (invitations, password resets, alerts).
function apply_system_mail() {
    if (env_str('CAN_MAIL_HOST') !== '') {
        $set = [
            'mail_driver' => 'smtp',
            'mail_host' => env_str('CAN_MAIL_HOST'),
            'mail_port' => env_str('CAN_MAIL_PORT'),
            'mail_username' => env_str('CAN_MAIL_USERNAME'),
            'mail_password' => app('encrypter')->encrypt(env_str('CAN_MAIL_PASSWORD'), false),
            'mail_encryption' => env_str('CAN_MAIL_ENCRYPTION'),
            'mail_from' => env_str('CAN_MAIL_FROM'),
        ];
        foreach ($set as $name => $value) {
            Option::set($name, $value);
        }
        Option::set('can_system_mail', 1);
        echo "can: system mail set to the company relay\n";
    } elseif (opt('can_system_mail')) {
        // Opted out, or the relay is gone: what was written here is removed again.
        foreach (['mail_driver', 'mail_host', 'mail_port', 'mail_username', 'mail_password', 'mail_encryption', 'mail_from', 'can_system_mail'] as $name) {
            Option::remove($name);
        }
        echo "can: system mail settings removed\n";
    }
}

// Removes what was written into one direction of a mailbox ("in" or "out").
function clear_connection($mailbox, $direction) {
    if ((int) opt("can_mailbox_$direction") !== (int) $mailbox->id) {
        return; // never written by us: an operator's own settings stay
    }
    if ($direction === 'out') {
        $mailbox->out_server = '';
        $mailbox->out_username = '';
        $mailbox->out_password = '';
    } else {
        $mailbox->in_server = '';
        $mailbox->in_username = '';
        $mailbox->in_password = '';
    }
    $mailbox->save();
    Option::remove("can_mailbox_$direction");
    echo "can: mailbox " . ($direction === 'out' ? 'outgoing' : 'incoming') . " settings removed\n";
}

function apply_mailbox() {
    $address = strtolower(trim(env_str('CAN_SUPPORT_ADDRESS')));
    $id = (int) opt('can_mailbox_id');
    $managed = $id ? Mailbox::find($id) : null;
    if ($address === '') {
        if ($managed) {
            clear_connection($managed, 'out');
            clear_connection($managed, 'in');
        }
        return;
    }
    if (User::where('email', $address)->exists()) {
        throw new ConfigRefused("support_address $address is the sign-in address of a FreeScout user; a mailbox needs an address of its own");
    }
    // A mailbox that already has the address (made by hand) is taken over; otherwise the one
    // made before is renamed, which keeps its conversations.
    $mailbox = Mailbox::where('email', $address)->first();
    if ($mailbox && $managed && $mailbox->id !== $managed->id) {
        clear_connection($managed, 'out');
        clear_connection($managed, 'in');
    }
    $mailbox = $mailbox ?: $managed ?: new Mailbox();
    $new = !$mailbox->exists;
    $mailbox->email = $address;
    $name = mb_substr(trim(strip_tags(env_str('CAN_SUPPORT_NAME'))), 0, 40);
    if ($name !== '') {
        $mailbox->name = $name;
    } elseif ($new) {
        $mailbox->name = 'Support';
    }

    $out = env_str('CAN_MAIL_HOST') !== '';
    if ($out) {
        $mailbox->out_method = Mailbox::OUT_METHOD_SMTP;
        $mailbox->out_server = env_str('CAN_MAIL_HOST');
        $mailbox->out_port = (int) env_str('CAN_MAIL_PORT');
        $mailbox->out_username = env_str('CAN_MAIL_USERNAME');
        $mailbox->out_password = env_str('CAN_MAIL_PASSWORD');
        $mailbox->out_encryption = env_str('CAN_MAIL_ENCRYPTION') === 'ssl' ? Mailbox::OUT_ENCRYPTION_SSL : Mailbox::OUT_ENCRYPTION_TLS;
    } elseif ($new) {
        // SMTP without a server reads as "not configured"; the default would try PHP mail().
        $mailbox->out_method = Mailbox::OUT_METHOD_SMTP;
    }

    $in = env_str('CAN_IMAP_HOST') !== '';
    if ($in) {
        $mailbox->in_protocol = Mailbox::IN_PROTOCOL_IMAP;
        $mailbox->in_server = env_str('CAN_IMAP_HOST');
        $mailbox->in_port = (int) env_str('CAN_IMAP_PORT');
        $mailbox->in_username = env_str('CAN_IMAP_USERNAME');
        $mailbox->in_password = env_str('CAN_IMAP_PASSWORD');
        $mailbox->in_encryption = env_str('CAN_IMAP_ENCRYPTION') === 'starttls' ? Mailbox::IN_ENCRYPTION_STARTTLS : Mailbox::IN_ENCRYPTION_SSL;
        $mailbox->in_validate_cert = env_str('CAN_IMAP_VERIFY') !== 'false';
        $mailbox->setInImapFolders([env_str('CAN_IMAP_FOLDER') ?: 'INBOX']);
    }
    $mailbox->save(); // a new mailbox gets its folders from FreeScout's observer
    if ($new) {
        $mailbox->syncPersonalFolders();
    }
    Option::set('can_mailbox_id', $mailbox->id);
    if ($out) {
        Option::set('can_mailbox_out', $mailbox->id);
    } else {
        clear_connection($mailbox, 'out');
    }
    if ($in) {
        Option::set('can_mailbox_in', $mailbox->id);
        if (!opt('can_alert_fetch')) {
            // Once: FreeScout mails its administrators when fetching stops. They may turn it off.
            Option::set('alert_fetch', true);
            Option::set('can_alert_fetch', 1);
        }
    } else {
        clear_connection($mailbox, 'in');
    }
    echo "can: mailbox $address " . ($new ? 'created' : 'updated') . " (outgoing: " . ($out ? 'configured' : 'none')
        . ", incoming: " . ($in ? 'configured' : 'none') . ")\n";
}

function apply() {
    try {
        apply_system_mail();
        apply_mailbox();
    } catch (\Throwable $e) {
        // Our own refusals are shown; anything else (a database error quotes values) by class only.
        $why = $e instanceof ConfigRefused ? $e->getMessage() : 'unexpected ' . get_class($e);
        Option::set('can_support_error', $why);
        fwrite(STDERR, "can: applying the mail settings failed: $why\n");
        exit(1);
    }
    Option::remove('can_support_error');
    // The queue worker caches settings; FreeScout restarts it the same way after a change.
    Illuminate\Support\Facades\Artisan::call('queue:restart');
}

function state($state, $detail = '') {
    return $detail === '' ? ['state' => $state] : ['state' => $state, 'detail' => $detail];
}

// Logs in to the mailbox now (nothing is read or marked) and looks at the last scheduled fetch.
function fetch_state($mailbox) {
    if (env_str('CAN_IMAP_HOST') === '') {
        return env_str('CAN_IMAP_WANTED') === '1'
            ? state('failing', 'no IMAP credentials: see the Secret can-system/can-support-imap')
            : state('not_configured');
    }
    if (!$mailbox) {
        return state('failing', 'there is no mailbox');
    }
    $test = \App\Misc\Mail::fetchTest($mailbox);
    if ($test['result'] !== 'success') {
        return state('failing', 'IMAP: ' . reason($test['msg']));
    }
    $last = (int) opt('fetch_emails_last_successful_run');
    if (!$last) {
        return state('pending', 'the first scheduled fetch has not finished');
    }
    if (time() - $last > 600) {
        return state('failing', 'no scheduled fetch has succeeded for ' . (int) ((time() - $last) / 60) . ' minutes');
    }
    return state('ok');
}

// Connects and signs in to the relay now (nothing is sent) and looks for replies that did not go.
function send_state($mailbox) {
    if (env_str('CAN_MAIL_HOST') === '') {
        return state('not_configured', 'no relay: email.modules.support.enabled and email.relay (docs/email.md)');
    }
    try {
        $transport = new Swift_SmtpTransport(env_str('CAN_MAIL_HOST'), (int) env_str('CAN_MAIL_PORT'), env_str('CAN_MAIL_ENCRYPTION'));
        $transport->setTimeout(15);
        if (env_str('CAN_MAIL_USERNAME') !== '') {
            $transport->setUsername(env_str('CAN_MAIL_USERNAME'));
            $transport->setPassword(env_str('CAN_MAIL_PASSWORD'));
        }
        $transport->start();
        $transport->stop();
    } catch (\Exception $e) {
        return state('failing', 'SMTP: ' . reason($e->getMessage()));
    }
    $stuck = \App\Job::where('queue', 'emails')->where('created_at', '<', time() - 900)->where('available_at', '<', time() - 900)->count();
    if ($stuck) {
        return state('failing', "$stuck queued message(s) have waited more than 15 minutes");
    }
    if ($mailbox) {
        $failed = \App\Thread::whereIn('send_status', [\App\SendLog::STATUS_SEND_ERROR, \App\SendLog::STATUS_SEND_INTERMEDIATE_ERROR])
            ->where('updated_at', '>', \Carbon\Carbon::now()->subDay())
            ->whereHas('conversation', function ($q) use ($mailbox) { $q->where('mailbox_id', $mailbox->id); })->count();
        if ($failed) {
            return state('failing', "$failed message(s) failed to send in the last day (see the conversation or Manage, Logs)");
        }
    }
    return state('ok');
}

// One JSON line; exit status 1 when something is failing.
function status() {
    $address = strtolower(trim(env_str('CAN_SUPPORT_ADDRESS')));
    $mailbox = $address === '' ? null : Mailbox::where('email', $address)->first();
    $error = (string) opt('can_support_error');
    if ($address === '') {
        $config = state('not_configured', 'modules.support.settings.support_address is not set');
    } elseif ($error !== '') {
        $config = state('failing', $error);
    } else {
        $config = $mailbox ? state('ok') : state('pending', 'the mailbox has not been created yet');
    }
    $out = ['config' => $config];
    $out['fetch'] = $address === '' ? state('not_configured') : fetch_state($mailbox);
    $out['send'] = $address === '' ? state('not_configured') : send_state($mailbox);
    echo "\n" . json_encode($out, JSON_UNESCAPED_SLASHES | JSON_INVALID_UTF8_SUBSTITUTE) . "\n";
    foreach ($out as $part) {
        if ($part['state'] === 'failing') {
            exit(1);
        }
    }
}

switch ($argv[1] ?? '') {
    case 'apply':
        apply();
        break;
    case 'status':
        status();
        break;
    default:
        fwrite(STDERR, "usage: can-support.php apply|status\n");
        exit(2);
}
{{- end -}}
